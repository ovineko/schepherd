package wrappers

import (
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// release writes the packages of a kind with their checksum file, as Build
// does, and returns the files in publication order.
func release(t *testing.T, dir, kind, version string) []string {
	t.Helper()

	v, err := VersionsOf(version)
	if err != nil {
		t.Fatal(err)
	}

	files := make([]string, 0, len(Targets)+1)

	for _, target := range Targets {
		if kind != Gem {
			files = append(files, map[string]string{Npm: NpmFile(target.NpmPackage(), v.SemVer), PyPI: target.WheelFile(v.PEP440)}[kind])
		}
	}

	files = append(files, map[string]string{Npm: NpmFile(NpmRoot, v.SemVer), Gem: GemFile(v.Gem)}[kind])
	files = slices.DeleteFunc(files, func(f string) bool { return f == "" })

	var sums strings.Builder

	for _, f := range files {
		writeFile(t, filepath.Join(dir, f), []byte("contents of "+f))
		fmt.Fprintf(&sums, "%s  %s\n", sha256Hex([]byte("contents of "+f)), f)
	}

	writeFile(t, filepath.Join(dir, "sums.txt"), []byte(sums.String()))

	return files
}

func unpublished(plan []Publication) []string {
	var files []string

	for _, p := range plan {
		if !p.Published {
			files = append(files, p.File)
		}
	}

	return files
}

func integrity(file string) string {
	sum := sha512.Sum512([]byte("contents of " + file))

	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func TestPlanNpmResumesAnInterruptedPublication(t *testing.T) {
	published := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pkg := strings.TrimPrefix(r.URL.Path, "/")
		if published[pkg] == "" {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = fmt.Fprintf(w, `{"versions":{"0.2.0-rc.1":{"dist":{"integrity":%q}}}}`, published[pkg])
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	files := release(t, dir, Npm, "0.2.0-rc.1")
	opts := PlanOptions{Kind: Npm, Dir: dir, Checksums: filepath.Join(dir, "sums.txt"), Version: "0.2.0-rc.1", Registry: server.URL}

	plan, err := Plan(t.Context(), opts)
	if err != nil || !slices.Equal(unpublished(plan), files) || files[len(files)-1] != "ovineko-schepherd-0.2.0-rc.1.tgz" {
		t.Fatalf("first plan %v, %v; want every file, the launcher last: %v", plan, err, files)
	}

	for _, target := range Targets[:2] {
		published[target.NpmPackage()] = integrity(NpmFile(target.NpmPackage(), "0.2.0-rc.1"))
	}

	if plan, err = Plan(t.Context(), opts); err != nil || !slices.Equal(unpublished(plan), files[2:]) || !plan[0].Published {
		t.Fatalf("resumed plan %v, %v; want %v", plan, err, files[2:])
	}

	published[Targets[3].NpmPackage()] = "sha512-other"
	if _, err := Plan(t.Context(), opts); err == nil || !strings.Contains(err.Error(), files[3]+" is already published with other contents") {
		t.Errorf("a version published with other bytes: %v", err)
	}
}

func TestPlanPyPIAndRubyGems(t *testing.T) {
	var answer, mediaType string

	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/simple/schepherd/" && r.URL.Path != "/api/v1/versions/schepherd.json" {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Type", mediaType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	wheels := release(t, filepath.Join(dir, "pypi"), PyPI, "1.0.0")
	gems := release(t, filepath.Join(dir, "gem"), Gem, "1.0.0")

	pypi := PlanOptions{Kind: PyPI, Dir: filepath.Join(dir, "pypi"), Checksums: filepath.Join(dir, "pypi", "sums.txt"), Version: "1.0.0", Registry: server.URL + "/simple/"}
	gem := PlanOptions{Kind: Gem, Dir: filepath.Join(dir, "gem"), Checksums: filepath.Join(dir, "gem", "sums.txt"), Version: "1.0.0", Registry: server.URL}

	status = http.StatusNotFound
	for _, opts := range []PlanOptions{pypi, gem} {
		if plan, err := Plan(t.Context(), opts); err != nil || len(unpublished(plan)) != len(plan) {
			t.Errorf("%s plan for a new project: %v, %v", opts.Kind, plan, err)
		}
	}

	status, mediaType = http.StatusOK, "application/vnd.pypi.simple.v1+json"
	answer = `{"files":[{"filename":"` + wheels[0] + `","hashes":{"sha256":"` + strings.ToUpper(sha256Hex([]byte("contents of "+wheels[0]))) + `"}}]}`

	if plan, err := Plan(t.Context(), pypi); err != nil || !slices.Equal(unpublished(plan), wheels[1:]) {
		t.Errorf("resumed PyPI plan %v, %v; want %v", plan, err, wheels[1:])
	}

	answer = `{"files":[{"filename":"` + wheels[1] + `","hashes":{"sha256":"` + strings.Repeat("0", 64) + `"}}]}`
	if _, err := Plan(t.Context(), pypi); err == nil || !strings.Contains(err.Error(), "already published with other contents") {
		t.Errorf("a wheel published with other bytes: %v", err)
	}

	mediaType = "text/html"
	if _, err := Plan(t.Context(), pypi); err == nil || !strings.Contains(err.Error(), "want application/vnd.pypi.simple.v1+json") {
		t.Errorf("an HTML answer: %v", err)
	}

	mediaType = "application/json; charset=utf-8"
	answer = `[{"number":"1.0.0","platform":"ruby","sha":"` + sha256Hex([]byte("contents of "+gems[0])) + `"}]`

	if plan, err := Plan(t.Context(), gem); err != nil || len(unpublished(plan)) != 0 {
		t.Errorf("a published gem: %v, %v", plan, err)
	}

	answer = `[{"number":"1.0.0","platform":"ruby","sha":"` + strings.Repeat("0", 64) + `"}]`
	if _, err := Plan(t.Context(), gem); err == nil || !strings.Contains(err.Error(), "already published with other contents") {
		t.Errorf("a gem published with other bytes: %v", err)
	}

	status = http.StatusInternalServerError
	if _, err := Plan(t.Context(), gem); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("a failing host: %v", err)
	}
}

func TestPlanRefuses(t *testing.T) {
	for want, change := range map[string]func(dir string, o *PlanOptions){
		"never published to pypi":   func(_ string, o *PlanOptions) { o.Version = "0.0.0-snapshot-abcdef1" },
		"unknown package kind":      func(_ string, o *PlanOptions) { o.Kind = "deb" },
		"no PEP 440 form":           func(_ string, o *PlanOptions) { o.Version = "1.0.0-preview.1" },
		"sums.txt lists":            func(_ string, o *PlanOptions) { o.Version = "1.0.1" },
		"differs from the checksum": func(dir string, _ *PlanOptions) { writeFile(t, filepath.Join(dir, Targets[0].WheelFile("1.0.0")), nil) },
		"but sums.txt lists":        func(dir string, _ *PlanOptions) { writeFile(t, filepath.Join(dir, "extra.whl"), nil) },
		"unexpected line":           func(dir string, _ *PlanOptions) { writeFile(t, filepath.Join(dir, "sums.txt"), []byte("sums\n")) },
	} {
		t.Run(want, func(t *testing.T) {
			dir := t.TempDir()
			release(t, dir, PyPI, "1.0.0")

			// The index never answers: every refusal comes before a query.
			o := PlanOptions{Kind: PyPI, Dir: dir, Checksums: filepath.Join(dir, "sums.txt"), Version: "1.0.0", Registry: "http://127.0.0.1:1/simple/"}
			change(dir, &o)

			if _, err := Plan(t.Context(), o); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%v, want an error containing %q", err, want)
			}
		})
	}
}
