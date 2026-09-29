package wrappers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Registries are the public registries the plans ask by default: the npm
// registry, the JSON simple API of PyPI (PEP 691) and the RubyGems host.
var Registries = map[string]string{Npm: "https://registry.npmjs.org", PyPI: "https://pypi.org/simple", Gem: "https://rubygems.org"}

// PlanOptions configures Plan.
type PlanOptions struct {
	Kind string
	// Dir holds the packages the checksum file lists and no other package
	// of the kind.
	Dir       string
	Checksums string
	Version   string
	// Registry replaces the entry of Registries for Kind.
	Registry string
	Client   *http.Client
}

// Publication is one package of a checksum file and its state on the
// registry.
type Publication struct {
	File   string
	SHA256 string
	// Published reports that the registry already has exactly this file.
	Published bool
}

var checksumLine = regexp.MustCompile(`^([0-9a-f]{64})  ([^/\\]+)$`)

// Plan checks the packages of a release against their checksum file, which
// must list exactly the files Build writes for the version, in publication
// order, and asks the registry which it already has, so running the plan
// again finishes an interrupted publication. A file the registry has with
// other bytes fails the plan, since a published version never changes, and
// so does a snapshot for PyPI and RubyGems.
func Plan(ctx context.Context, opts PlanOptions) ([]Publication, error) {
	v, err := VersionsOf(opts.Version)
	if err != nil {
		return nil, err
	}

	files := map[string][]string{Gem: {GemFile(v.Gem)}}
	for _, t := range Targets {
		files[Npm] = append(files[Npm], NpmFile(t.NpmPackage(), v.SemVer))
		files[PyPI] = append(files[PyPI], t.WheelFile(v.PEP440))
	}

	files[Npm] = append(files[Npm], NpmFile(NpmRoot, v.SemVer))

	switch {
	case files[opts.Kind] == nil:
		return nil, fmt.Errorf("unknown package kind %q", opts.Kind)
	case v.Snapshot && opts.Kind != Npm:
		return nil, fmt.Errorf("%s is a snapshot version, which is never published to %s", opts.Version, opts.Kind)
	}

	plan, err := readChecksums(opts, files[opts.Kind])
	if err != nil {
		return nil, err
	}

	var conflicts []error

	for i, p := range plan {
		local, remote, err := digests(ctx, opts, v, p)

		switch {
		case err != nil:
			return nil, err
		case remote == local:
			plan[i].Published = true
		case remote != "":
			conflicts = append(conflicts, fmt.Errorf("%s is already published with other contents; a published version never changes, so release a new version", p.File))
		}
	}

	return plan, errors.Join(conflicts...)
}

// readChecksums requires the checksum file to list exactly want, in that
// order, and Dir to hold exactly those files of the kind with the listed
// SHA-256.
func readChecksums(opts PlanOptions, want []string) ([]Publication, error) {
	data, err := os.ReadFile(filepath.Clean(opts.Checksums))
	if err != nil {
		return nil, fmt.Errorf("read checksum file: %w", err)
	}

	var (
		plan  []Publication
		files []string
	)

	for scanner := bufio.NewScanner(bytes.NewReader(data)); scanner.Scan(); {
		m := checksumLine.FindStringSubmatch(scanner.Text())
		if m == nil {
			return nil, fmt.Errorf("%s: unexpected line %q, want <sha256>  <file>", filepath.Base(opts.Checksums), scanner.Text())
		}

		plan = append(plan, Publication{File: m[2], SHA256: m[1]})
		files = append(files, m[2])
	}

	present, err := filepath.Glob(filepath.Join(opts.Dir, "*"+filepath.Ext(want[0])))
	for i := range present {
		present[i] = filepath.Base(present[i])
	}

	switch {
	case err != nil:
		return nil, fmt.Errorf("list %s: %w", opts.Dir, err)
	case !slices.Equal(files, want):
		return nil, fmt.Errorf("%s lists %v, want %v", filepath.Base(opts.Checksums), files, want)
	case !slices.Equal(slices.Sorted(slices.Values(present)), slices.Sorted(slices.Values(want))):
		return nil, fmt.Errorf("%s holds %v, but %s lists %v", opts.Dir, present, filepath.Base(opts.Checksums), want)
	}

	for _, p := range plan {
		content, err := os.ReadFile(filepath.Clean(filepath.Join(opts.Dir, p.File)))
		if err != nil {
			return nil, fmt.Errorf("read package: %w", err)
		}

		if got := sha256Hex(content); got != p.SHA256 {
			return nil, fmt.Errorf("%s: SHA-256 %s differs from the checksum file (%s)", p.File, got, p.SHA256)
		}
	}

	return plan, nil
}

type (
	npmDocument struct {
		Versions map[string]struct {
			Dist struct {
				Integrity string `json:"integrity"`
			} `json:"dist"`
		} `json:"versions"`
	}
	pypiProject struct {
		Files []struct {
			Filename string            `json:"filename"`
			Hashes   map[string]string `json:"hashes"`
		} `json:"files"`
	}
	gemVersions []struct {
		Number   string `json:"number"`
		Platform string `json:"platform"`
		SHA      string `json:"sha"`
	}
)

// digests returns the digest of a package in the form its registry records,
// and the one the registry has for it, empty when it has none: the npm
// dist.integrity (SHA-512) of the version, the SHA-256 PyPI lists for the
// file, or the SHA-256 RubyGems records for the version.
func digests(ctx context.Context, opts PlanOptions, v Versions, p Publication) (string, string, error) {
	base := opts.Registry
	if base == "" {
		base = Registries[opts.Kind]
	}

	base = strings.TrimSuffix(base, "/")

	switch opts.Kind {
	case Npm:
		data, err := os.ReadFile(filepath.Clean(filepath.Join(opts.Dir, p.File)))
		if err != nil {
			return "", "", fmt.Errorf("read package: %w", err)
		}

		var doc npmDocument

		pkg := "@" + strings.Replace(strings.TrimSuffix(p.File, "-"+v.SemVer+".tgz"), "-", "/", 1)
		err = getJSON(ctx, opts, base+"/"+url.PathEscape(pkg), "application/json", &doc)
		sum := sha512.Sum512(data)

		return "sha512-" + base64.StdEncoding.EncodeToString(sum[:]), doc.Versions[v.SemVer].Dist.Integrity, err
	case PyPI:
		var project pypiProject

		err := getJSON(ctx, opts, base+"/"+BinaryName+"/", "application/vnd.pypi.simple.v1+json", &project)

		for _, f := range project.Files {
			if f.Filename == p.File {
				return p.SHA256, strings.ToLower(f.Hashes["sha256"]), err
			}
		}

		return p.SHA256, "", err
	default:
		var versions gemVersions

		err := getJSON(ctx, opts, base+"/api/v1/versions/"+BinaryName+".json", "application/json", &versions)

		for _, r := range versions {
			if r.Number == v.Gem && r.Platform == "ruby" {
				return p.SHA256, r.SHA, err
			}
		}

		return p.SHA256, "", err
	}
}

// maxAnswer bounds an answer of a registry.
const maxAnswer = 64 << 20

// getJSON decodes the answer of a registry into v, and leaves v alone when
// the registry does not know the package.
func getJSON(ctx context.Context, opts PlanOptions, endpoint, accept string, v any) error {
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: time.Minute}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("query %s: %w", endpoint, err)
	}

	req.Header.Set("Accept", accept)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("query %s: %w", endpoint, err)
	}

	defer func() { _ = resp.Body.Close() }()

	switch mediaType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";"); {
	case resp.StatusCode == http.StatusNotFound:
		return nil
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("query %s: HTTP %s", endpoint, resp.Status)
	case strings.TrimSpace(mediaType) != accept:
		return fmt.Errorf("query %s: the answer is %q, want %s", endpoint, mediaType, accept)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err == nil && len(body) > maxAnswer {
		err = fmt.Errorf("the answer exceeds %d bytes", maxAnswer)
	}

	if err == nil {
		err = json.Unmarshal(body, v)
	}

	if err != nil {
		return fmt.Errorf("query %s: %w", endpoint, err)
	}

	return nil
}
