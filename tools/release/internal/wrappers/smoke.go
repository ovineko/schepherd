package wrappers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// smoke installs the packages of the running platform with npm, uv and gem
// install, offline, and runs each launcher: `version --json` and an offline
// command that fails with exit status 6 must give exactly the output and
// exit status of the GoReleaser binary run directly. With RubyImage the gem
// is installed and run in that container, with the linux binary of the
// host's architecture.
func (b *builder) smoke(ctx context.Context) error {
	for _, kind := range b.Kinds {
		r := Runner{Dir: filepath.Join(b.tmp, "smoke-"+kind), Env: b.Env}
		if kind == Gem {
			r.Image = b.RubyImage
		}

		goos, goarch := r.Platform()

		i := slices.IndexFunc(b.binaries, func(d DistBinary) bool { return d.Target.GOOS == goos && d.Target.GOARCH == goarch })
		if i < 0 {
			return fmt.Errorf("%s smoke: the dist has no %s/%s binary", kind, goos, goarch)
		}

		if err := os.Mkdir(r.Dir, 0o750); err != nil {
			return fmt.Errorf("create smoke directory: %w", err)
		}

		launcher, env, err := install(ctx, r, kind, filepath.Join(b.Out, kind), b.v, b.binaries[i].Target)
		if err == nil {
			err = compare(ctx, r, b.binaries[i], launcher, env)
		}

		if err != nil {
			return fmt.Errorf("%s smoke: %w", kind, err)
		}
	}

	return nil
}

// install installs the package of t in r.Dir and returns the command that
// runs its launcher and the environment it needs.
func install(ctx context.Context, r Runner, kind, dir string, v Versions, t Target) ([]string, []string, error) {
	switch kind {
	case Npm:
		if err := os.WriteFile(filepath.Join(r.Dir, "package.json"), []byte(`{"name":"smoke","version":"1.0.0","private":true}`), 0o600); err != nil {
			return nil, nil, fmt.Errorf("write package.json: %w", err)
		}

		// The launcher and the platform package only: the other optional
		// dependencies would come from a registry, which the smoke never uses.
		if _, err := r.Must(ctx, nil, "npm", "install", "--offline", "--no-audit", "--no-fund", "--ignore-scripts", "--omit=optional",
			filepath.Join(dir, NpmFile(NpmRoot, v.SemVer)), filepath.Join(dir, NpmFile(t.NpmPackage(), v.SemVer))); err != nil {
			return nil, nil, err
		}

		return []string{"node", filepath.Join(r.Dir, "node_modules", "@ovineko", "schepherd", "bin", "schepherd.js")}, nil, nil
	case PyPI:
		scripts, python, exe := "bin", "python", ""
		if runtime.GOOS == "windows" {
			scripts, python, exe = "Scripts", "python.exe", ".exe"
		}

		venv := filepath.Join(r.Dir, "venv")
		if _, err := r.Must(ctx, nil, "uv", "venv", "--quiet", venv); err != nil {
			return nil, nil, err
		}

		if _, err := r.Must(ctx, nil, "uv", "pip", "install", "--quiet", "--offline", "--no-deps", "--python", filepath.Join(venv, scripts, python),
			filepath.Join(dir, t.WheelFile(v.PEP440))); err != nil {
			return nil, nil, err
		}

		return []string{filepath.Join(venv, scripts, BinaryName+exe)}, nil, nil
	default:
		// A container sees only r.Dir.
		gem := filepath.Clean(filepath.Join(r.Dir, GemFile(v.Gem)))

		data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, GemFile(v.Gem))))
		//nolint:gosec // G703: the file name comes from the version, the directory from os.MkdirTemp
		if err := errors.Join(err, os.WriteFile(gem, data, 0o600)); err != nil {
			return nil, nil, fmt.Errorf("copy the gem: %w", err)
		}

		gems := filepath.Join(r.Dir, "gems")
		if _, err := r.Must(ctx, nil, "ruby", "-S", "gem", "install", "--local", "--norc", "--no-document", "--ignore-dependencies",
			"--install-dir", gems, "--bindir", filepath.Join(gems, "bin"), gem); err != nil {
			return nil, nil, err
		}

		// ruby runs the executable the same way on Windows, where RubyGems
		// adds a .bat file next to it.
		return []string{"ruby", filepath.Join(gems, "bin", BinaryName)}, []string{"GEM_HOME=" + gems, "GEM_PATH=" + gems, "SCHEPHERD_BINARY="}, nil
	}
}

// compare runs the launcher and a copy of the binary in r.Dir with the same
// arguments and requires the same outcome.
func compare(ctx context.Context, r Runner, bin DistBinary, launcher, env []string) error {
	data, err := os.ReadFile(bin.Path)
	if err != nil {
		return fmt.Errorf("read the binary: %w", err)
	}

	direct := filepath.Join(r.Dir, "direct", bin.Target.Binary())
	// bearer:disable go_gosec_file_permissions_file_perm
	// The smoke test runs this copy of the binary, so it must be executable.
	if err := errors.Join(os.MkdirAll(filepath.Dir(direct), 0o750), os.WriteFile(direct, data, 0o700)); err != nil { //nolint:gosec // G306: an executable
		return fmt.Errorf("copy the binary: %w", err)
	}

	failing := []string{
		"--offline", "--cache-dir", filepath.Join(r.Dir, "cache"), "--repository", "example.invalid/org/schemas",
		"--catalog", "sha256:" + strings.Repeat("0", 64), "list",
	}

	for _, args := range [][]string{{"version", "--json"}, failing} {
		wantOut, wantErr, wantCode, err := r.Run(ctx, env, append([]string{direct}, args...)...)
		if err != nil {
			return err
		}

		gotOut, gotErr, gotCode, err := r.Run(ctx, env, slices.Concat(launcher, args)...)
		if err != nil {
			return err
		}

		status := 6
		if args[0] == "version" {
			status = 0
		}

		if wantCode != status {
			return fmt.Errorf("the binary exited %d for %v: %s", wantCode, args, wantErr)
		}

		if gotCode != wantCode || !bytes.Equal(gotOut, wantOut) || !bytes.Equal(gotErr, wantErr) {
			return fmt.Errorf("%s %s exited %d with %q %q; the binary exited %d with %q %q",
				strings.Join(launcher, " "), strings.Join(args, " "), gotCode, gotOut, gotErr, wantCode, wantOut, wantErr)
		}
	}

	return nil
}
