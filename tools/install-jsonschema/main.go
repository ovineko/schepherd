// Command install-jsonschema installs the pinned Sourcemeta JSON Schema CLI
// for the host platform into a project-local directory (default .tools/bin
// under the module root) and prints the installed path.
//
// The archive is downloaded from the upstream release, verified against the
// SHA-256 recorded in tools/pins/jsonschema.json before it is opened, and only
// the executable is extracted. The CLI is AGPL-3.0 licensed: it is a
// maintainer-side tool run as a subprocess and is never shipped.
package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ovineko/schepherd/tools/pins"
)

const (
	maxArchiveBytes = 128 << 20
	maxBinaryBytes  = 256 << 20
	downloadTimeout = 10 * time.Minute
	versionTimeout  = 30 * time.Second
	executableMode  = 0o755
)

type options struct {
	dest     string
	platform string
	force    bool
}

// installer holds the dependencies tests replace: the HTTP client and the
// size limits applied to the archive and the extracted executable.
type installer struct {
	client       *http.Client
	archiveLimit int64
	binaryLimit  int64
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)

	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("install-jsonschema", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var opts options
	flags.StringVar(&opts.dest, "dest", "", "installation directory (default: <module root>/.tools/bin)")
	flags.StringVar(&opts.platform, "platform", "", "release platform such as linux-x86_64 (default: detected)")
	flags.BoolVar(&opts.force, "force", false, "download again even if the pinned version is already installed")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	if flags.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "install-jsonschema: unexpected arguments %q\n", flags.Args())

		return 2
	}

	in := installer{client: http.DefaultClient, archiveLimit: maxArchiveBytes, binaryLimit: maxBinaryBytes}

	installed, err := in.install(ctx, opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "install-jsonschema: %v\n", err)

		return 1
	}

	_, _ = fmt.Fprintln(stdout, installed)

	return 0
}

func (in installer) install(ctx context.Context, opts options) (string, error) {
	tool, err := pins.JSONSchema()
	if err != nil {
		return "", fmt.Errorf("read pins: %w", err)
	}

	platform := opts.platform
	if platform == "" {
		platform, err = hostPlatform(runtime.GOOS, runtime.GOARCH, fileExists)
		if err != nil {
			return "", err
		}
	}

	link, asset, err := tool.AssetURL(platform)
	if err != nil {
		return "", fmt.Errorf("select release asset: %w", err)
	}

	dest := opts.dest
	if dest == "" {
		root, err := moduleRoot()
		if err != nil {
			return "", err
		}

		dest = filepath.Join(root, ".tools", "bin")
	}

	dest, err = filepath.Abs(dest)
	if err != nil {
		return "", fmt.Errorf("resolve destination: %w", err)
	}

	target := filepath.Join(dest, binaryName(asset.Binary))

	if !opts.force {
		if version, err := binaryVersion(ctx, target); err == nil && version == tool.Version {
			return target, nil
		}
	}

	if err := os.MkdirAll(dest, 0o750); err != nil {
		return "", fmt.Errorf("create destination: %w", err)
	}

	archive, err := in.download(ctx, link, asset.SHA256, dest)
	if err != nil {
		return "", err
	}

	defer func() { _ = os.Remove(archive) }()

	if err := extract(archive, asset.Binary, target, in.binaryLimit); err != nil {
		return "", err
	}

	version, err := binaryVersion(ctx, target)
	if err != nil {
		return "", fmt.Errorf("run installed binary: %w", err)
	}

	if version != tool.Version {
		return "", fmt.Errorf("installed binary reports version %q, want %q", version, tool.Version)
	}

	return target, nil
}

// hostPlatform maps a Go target to a release platform name. On Linux the musl
// build is chosen only when the glibc loader is absent and a musl loader is
// present, because the glibc build is the one the release notes describe as
// the default.
func hostPlatform(goos, goarch string, exists func(string) bool) (string, error) {
	arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[goarch]
	if arch == "" {
		return "", fmt.Errorf("no pinned release for %s/%s", goos, goarch)
	}

	switch goos {
	case "darwin":
		return "darwin-" + arch, nil
	case "windows":
		if arch != "x86_64" {
			return "", fmt.Errorf("no pinned release for %s/%s", goos, goarch)
		}

		return "windows-" + arch, nil
	case "linux":
		loaders := map[string][2]string{
			"x86_64": {"/lib64/ld-linux-x86-64.so.2", "/lib/ld-musl-x86_64.so.1"},
			"arm64":  {"/lib/ld-linux-aarch64.so.1", "/lib/ld-musl-aarch64.so.1"},
		}[arch]

		if !exists(loaders[0]) && exists(loaders[1]) {
			return "linux-" + arch + "-musl", nil
		}

		return "linux-" + arch, nil
	default:
		return "", fmt.Errorf("no pinned release for %s/%s", goos, goarch)
	}
}

func fileExists(name string) bool {
	_, err := os.Stat(name)

	return err == nil
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}

	for {
		if fileExists(filepath.Join(dir, "go.mod")) {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found in the working directory or its parents; pass -dest")
		}

		dir = parent
	}
}

func binaryName(binary string) string {
	return filepath.Base(filepath.FromSlash(binary))
}

func binaryVersion(ctx context.Context, binary string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()

	// bearer:disable go_gosec_injection_subproc_injection
	// binary is the Sourcemeta CLI this tool installs, whose download matched the pinned SHA-256; it runs without a shell.
	out, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", binary, err)
	}

	return strings.TrimSpace(string(out)), nil
}

func (in installer) download(ctx context.Context, link, wantSHA256, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}

	resp, err := in.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", link, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %s", link, resp.Status)
	}

	file, err := os.CreateTemp(dir, ".jsonschema-*.zip")
	if err != nil {
		return "", fmt.Errorf("create archive file: %w", err)
	}

	name := file.Name()
	hash := sha256.New()

	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(resp.Body, in.archiveLimit+1))

	closeErr := file.Close()

	switch {
	case copyErr != nil:
		err = fmt.Errorf("download %s: %w", link, copyErr)
	case closeErr != nil:
		err = fmt.Errorf("write archive: %w", closeErr)
	case written > in.archiveLimit:
		err = fmt.Errorf("download %s: archive exceeds %d bytes", link, in.archiveLimit)
	default:
		if got := hex.EncodeToString(hash.Sum(nil)); got != wantSHA256 {
			err = fmt.Errorf("download %s: sha256 %s does not match pinned %s", link, got, wantSHA256)
		}
	}

	if err != nil {
		_ = os.Remove(name)

		return "", err
	}

	return name, nil
}

func extract(archive, entry, target string, limit int64) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}

	defer func() { _ = reader.Close() }()

	for _, file := range reader.File {
		if file.Name != entry {
			continue
		}

		return writeEntry(file, target, limit)
	}

	return fmt.Errorf("archive has no entry %s", entry)
}

func writeEntry(file *zip.File, target string, limit int64) error {
	src, err := file.Open()
	if err != nil {
		return fmt.Errorf("open archive entry: %w", err)
	}

	defer func() { _ = src.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(target), ".jsonschema-*")
	if err != nil {
		return fmt.Errorf("create binary: %w", err)
	}

	name := tmp.Name()

	written, copyErr := io.Copy(tmp, io.LimitReader(src, limit+1))
	closeErr := tmp.Close()

	switch {
	case copyErr != nil:
		err = fmt.Errorf("extract %s: %w", file.Name, copyErr)
	case closeErr != nil:
		err = fmt.Errorf("write binary: %w", closeErr)
	case written > limit:
		err = fmt.Errorf("extract %s: entry exceeds %d bytes", file.Name, limit)
	default:
		err = place(name, target)
	}

	if err != nil {
		_ = os.Remove(name)

		return err
	}

	return nil
}

func place(name, target string) error {
	if err := os.Chmod(name, executableMode); err != nil {
		return fmt.Errorf("make binary executable: %w", err)
	}

	if err := os.Rename(name, target); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}

	return nil
}
