//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// releaseNpmPackages are the npm packages of a release: the launcher
// package @ovineko/schepherd and one @ovineko/schepherd-<os>-<cpu> package
// per supported platform (packaging/npm/schepherd/README.md).
var releaseNpmPackages = []string{
	"schepherd",
	"schepherd-darwin-arm64", "schepherd-darwin-x64",
	"schepherd-linux-arm64", "schepherd-linux-x64",
	"schepherd-win32-arm64", "schepherd-win32-x64",
}

// releaseNpmrc authenticates to verdaccio with a token it does not know;
// the npm profile's policy lets anonymous users publish, and npm refuses to
// publish without any token.
const releaseNpmrc = "registry=http://verdaccio:4873/\n//verdaccio:4873/:_authToken=e2e-dummy-token\n"

// releaseOfflineConfig names a catalog that no cache contains.
var releaseOfflineConfig = "config_version = 1\n\n[catalog]\nrepository = \"registry.invalid/e2e/schemas\"\ndigest = \"sha256:" +
	strings.Repeat("a", 64) + "\"\n"

// TestE37_NpmPackaging builds every release target with a GoReleaser
// snapshot and turns the binaries into the npm tarballs of its snapshot
// version 0.0.0-snapshot-<commit> with `packages build`, as the release
// workflow's build job does, and publishes them to verdaccio under the
// dist-tag next the way its publish-npm job publishes a pre-release: in the
// order of `packages publish-plan`, first interrupted after two packages,
// then resumed from a new plan that skips what the registry has, until the
// plan is empty; a tarball whose bytes differ from a published version fails
// the plan. It then installs @ovineko/schepherd into a fresh project with the
// Node image: only the platform package of the container's system is
// installed, with the repository's THIRD_PARTY_LICENSES.txt, the snapshot
// binary reports "dev", and the launcher passes stdin to the native binary
// and its output and exit status back, under Node (npx and the bin link) and
// under Bun, without any network.
func TestE37_NpmPackaging(t *testing.T) {
	t.Parallel()

	base := npmRegistry(t)
	work := t.TempDir()
	src := filepath.Join(work, "src")

	releaseCopyWorktree(t, src)
	commit := releaseCommitSnapshot(t, src)

	hostTool(t, runOpts{Dir: src, Env: releaseGitEnv(), Timeout: 30 * time.Minute},
		".tools/bin/goreleaser", "build", "--snapshot", "--clean").ok(t)

	version := releaseSnapshotVersion(t, src, commit)
	packages := filepath.Join(work, "packages")

	releaseTool(t, src, "packages", "build", "--dist", "dist", "--version", version, "--out", packages, "--kinds", "npm")

	npmrc := filepath.Join(work, "npmrc")
	writeFile(t, filepath.Join(npmrc, "npmrc"), []byte(releaseNpmrc))

	releaseNpmPublish(t, src, packages, npmrc, work, base, version)
	releaseCheckNpmRegistry(t, base, version)

	project := filepath.Join(work, "project")
	writeFile(t, filepath.Join(project, "package.json"), []byte(`{"name":"e2e-npm-consumer","version":"1.0.0","private":true}`+"\n"))
	writeFile(t, filepath.Join(project, "schepherd.toml"), []byte(releaseOfflineConfig))

	dockerRun(t, nodeImage, dockerOpts{
		Network: composeNetwork(),
		Mounts:  []mount{{Host: project, Container: "/e2e/project", Writable: true}, {Host: npmrc, Container: "/e2e/npmrc"}},
		Workdir: "/e2e/project",
		Env:     releaseNpmEnv(),
	}, "npm", "install", "--save-dev", "@ovineko/schepherd@"+version).ok(t)

	platform := strings.TrimSpace(string(dockerRun(t, nodeImage, dockerOpts{}, "node", "-p", `process.platform + "-" + process.arch`).ok(t).Stdout))
	installed := releaseCheckInstalled(t, project, src, platform)
	t.Logf("published %d packages at %s to verdaccio; the %s container installed only @ovineko/schepherd and @ovineko/schepherd-%s",
		len(releaseNpmPackages), version, platform, platform)

	releaseCheckLauncher(t, project, filepath.Dir(installed), commit)
}

// releaseTool runs the release tooling of the snapshot's source tree on the
// host.
func releaseTool(t *testing.T, src string, args ...string) result {
	t.Helper()

	return hostTool(t, runOpts{Dir: src, Env: append(releaseGitEnv(), "GOWORK=off"), Timeout: 10 * time.Minute},
		"go", append([]string{"run", "./tools/release"}, args...)...).ok(t)
}

// releaseSnapshotVersion returns the version GoReleaser gave the snapshot
// of commit in src/dist.
func releaseSnapshotVersion(t *testing.T, src, commit string) string {
	t.Helper()

	version := strings.TrimSpace(string(releaseTool(t, src, "version", "snapshot", "--dist", "dist").Stdout))
	if want := "0.0.0-snapshot-" + commit[:7]; !strings.HasPrefix(version, want) {
		t.Fatalf("the snapshot version is %q, want %s<more of the commit>", version, want)
	}

	return version
}

// releaseCheckInstalled asserts that npm installed the launcher package and
// exactly the platform package of the container, whose binary is the one
// GoReleaser built for that platform, and returns the installed binary.
func releaseCheckInstalled(t *testing.T, project, src, platform string) string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(project, "node_modules", "@ovineko"))
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}

	pkg := "schepherd-" + platform
	if want := []string{"schepherd", pkg}; !slices.Equal(got, want) {
		t.Fatalf("installed @ovineko packages %v, want only %v", got, want)
	}

	installed := filepath.Join(project, "node_modules", "@ovineko", pkg, "bin", "schepherd")

	info, err := os.Stat(installed)
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed binary %s: %v (mode %v)", installed, err, info)
	}

	sum := sha256Hex(readFile(t, installed))
	if want := sha256Hex(readFile(t, releaseDistBinary(t, src, platform))); sum != want {
		t.Fatalf("installed binary sha256 %s, GoReleaser built %s", sum, want)
	}

	licenses := filepath.Join(project, "node_modules", "@ovineko", pkg, "THIRD_PARTY_LICENSES.txt")
	if got, want := readFile(t, licenses), readFile(t, filepath.Join(src, "THIRD_PARTY_LICENSES.txt")); !bytes.Equal(got, want) {
		t.Fatalf("installed %s (%d bytes) differs from the repository's THIRD_PARTY_LICENSES.txt (%d bytes)", licenses, len(got), len(want))
	}

	return installed
}

// releaseCheckLauncher runs the installed launcher under Node (npx and the
// node_modules/.bin link) and under Bun, each without network, and compares
// stdout, stderr and exit status with the native binary run directly: once
// for version --json and once for an offline cache miss (exit 6). Each
// launcher must also pass stdin through to the binary unchanged.
func releaseCheckLauncher(t *testing.T, project, binDir, commit string) {
	t.Helper()

	opts := dockerOpts{
		Mounts:  []mount{{Host: project, Container: "/e2e/project"}, {Host: binDir, Container: "/e2e/direct"}},
		Workdir: "/e2e/project",
		Env:     releaseRunEnv(),
	}

	versionArgs := []string{"version", "--json"}
	failArgs := []string{"--offline", "--config", "/e2e/project/schepherd.toml", "path", "alpha"}

	direct := dockerRun(t, nodeImage, opts, append([]string{"/e2e/direct/schepherd"}, versionArgs...)...).ok(t)
	directFail := dockerRun(t, nodeImage, opts, append([]string{"/e2e/direct/schepherd"}, failArgs...)...).wantCode(t, 6)

	info := decodeJSON[struct {
		Version               string `json:"version"`
		Commit                string `json:"commit"`
		Release               bool   `json:"release"`
		CatalogFormatVersions []int  `json:"catalogFormatVersions"`
	}](t, direct.Stdout)
	if info.Version != "dev" || info.Release || info.Commit != commit || !slices.Equal(info.CatalogFormatVersions, []int{2}) {
		t.Fatalf("the snapshot binary reports %+v, want version dev, no release, commit %s, catalog format 1", info, commit)
	}

	if len(directFail.Stdout) != 0 || !bytes.Contains(directFail.Stderr, []byte("sha256:"+strings.Repeat("a", 64))) {
		t.Fatalf("offline miss of the native binary:\n%s", directFail)
	}

	launchers := []struct {
		name   string
		image  string
		prefix []string
	}{
		{"npx", nodeImage, []string{"npx", "--no", "--", "schepherd"}},
		{"node_modules/.bin link", nodeImage, []string{"./node_modules/.bin/schepherd"}},
		{"bun", bunImage, []string{"bun", "node_modules/@ovineko/schepherd/bin/schepherd.js"}},
	}

	payload := releaseStdinPayload()

	for _, l := range launchers {
		for _, c := range []struct {
			args []string
			want result
		}{{versionArgs, direct}, {failArgs, directFail}} {
			got := dockerRun(t, l.image, opts, append(slices.Clone(l.prefix), c.args...)...)
			if got.Code != c.want.Code || !bytes.Equal(got.Stdout, c.want.Stdout) || !bytes.Equal(got.Stderr, c.want.Stderr) {
				t.Errorf("%s does not pass the binary's outcome through\n--- launcher ---\n%s\n--- binary ---\n%s", l.name, got, c.want)
			}
		}

		releaseCheckLauncherStdin(t, l.name, l.image, l.prefix, opts, payload)
	}
}

// releaseStdinPayload is larger than a pipe buffer and holds every byte
// value, so a launcher that read, decoded or truncated stdin changes it.
func releaseStdinPayload() []byte {
	payload := make([]byte, 1<<20+7)
	for i := range payload {
		payload[i] = byte(i*131 + i>>8)
	}

	return payload
}

// releaseCheckLauncherStdin runs a launcher with SCHEPHERD_BINARY pointing
// at the testconsumer, which records what it read from stdin.
func releaseCheckLauncherStdin(t *testing.T, name, image string, prefix []string, base dockerOpts, payload []byte) {
	t.Helper()

	logDir := t.TempDir()
	opts := base
	opts.Mounts = append(slices.Clone(base.Mounts), binMount(), mount{Host: logDir, Container: "/e2e/log", Writable: true})
	opts.Env = append(slices.Clone(base.Env), "SCHEPHERD_BINARY=/e2e/bin/testconsumer", "TC_READ_STDIN=1", "TC_LOG_DIR=/e2e/log")
	opts.Stdin = payload

	if got := dockerRun(t, image, opts, prefix...); got.Code != 0 {
		t.Errorf("%s with a %d-byte stdin exited %d, want 0\n%s", name, len(payload), got.Code, got)

		return
	}

	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatal(err)
	}

	var records []string

	for _, e := range entries {
		if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") && strings.HasSuffix(e.Name(), ".json") {
			records = append(records, filepath.Join(logDir, e.Name()))
		}
	}

	if len(records) != 1 {
		t.Errorf("%s: the binary wrote %d records, want exactly 1", name, len(records))

		return
	}

	rec := decodeJSON[struct {
		Stdin *string `json:"stdin"`
	}](t, readFile(t, records[0]))
	if rec.Stdin == nil {
		t.Errorf("%s: the binary's record has no stdin", name)

		return
	}

	got, err := base64.StdEncoding.DecodeString(*rec.Stdin)
	if err != nil || !bytes.Equal(got, payload) {
		t.Errorf("%s: the binary read %d bytes (sha256 %s) from stdin, want the %d-byte payload (sha256 %s): %v",
			name, len(got), sha256Hex(got), len(payload), sha256Hex(payload), err)
	}
}

// releaseCopyWorktree copies what Git would see in the repository's working
// tree (tracked and untracked, non-ignored files) to dst, so GoReleaser can
// build in a throwaway repository without touching the developer's dist/.
func releaseCopyWorktree(t *testing.T, dst string) {
	t.Helper()

	res := hostTool(t, runOpts{}, "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard").ok(t)

	seen := map[string]bool{}

	for rel := range strings.SplitSeq(strings.TrimRight(string(res.Stdout), "\x00"), "\x00") {
		if rel == "" || seen[rel] {
			continue
		}

		seen[rel] = true
		src := filepath.Join(suite.root, filepath.FromSlash(rel))
		target := filepath.Join(dst, filepath.FromSlash(rel))

		info, err := os.Lstat(src)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}

		if err != nil {
			t.Fatal(err)
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}

		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(src)
			if err == nil {
				err = os.Symlink(link, target)
			}

			if err != nil {
				t.Fatal(err)
			}
		case info.Mode().IsRegular():
			if err := os.WriteFile(target, readFile(t, src), info.Mode().Perm()); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("%s is neither a file nor a symbolic link", rel)
		}
	}
}

// releaseGitEnv isolates Git from the developer's configuration and gives
// commits a test identity.
func releaseGitEnv() []string {
	return []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Schepherd E2E",
		"GIT_AUTHOR_EMAIL=e2e@example.invalid",
		"GIT_COMMITTER_NAME=Schepherd E2E",
		"GIT_COMMITTER_EMAIL=e2e@example.invalid",
	}
}

// releaseCommitSnapshot makes dir a repository with one commit and returns
// the commit ID. GoReleaser reads the commit, its date and the remote URL; it
// drops all Git metadata when no remote is configured, so the public URL is
// added as origin. Nothing is ever pushed.
func releaseCommitSnapshot(t *testing.T, dir string) string {
	t.Helper()

	git := func(args ...string) result {
		t.Helper()

		return hostTool(t, runOpts{Dir: dir, Env: releaseGitEnv()}, "git", args...).ok(t)
	}

	git("init", "-q", "-b", "main")
	git("add", "-A")
	git("-c", "commit.gpgsign=false", "commit", "-q", "-m", "e2e npm packaging snapshot")
	git("remote", "add", "origin", "https://github.com/ovineko/schepherd.git")

	return strings.TrimSpace(string(git("rev-parse", "HEAD").Stdout))
}

// releaseDistBinary finds the schepherd binary GoReleaser built for an npm
// platform (os-cpu) in src/dist/artifacts.json.
func releaseDistBinary(t *testing.T, src, platform string) string {
	t.Helper()

	goos, cpu, _ := strings.Cut(platform, "-")
	goarch := map[string]string{"x64": "amd64", "arm64": "arm64"}[cpu]

	artifacts := decodeJSON[[]struct {
		Type   string `json:"type"`
		Path   string `json:"path"`
		GOOS   string `json:"goos"`
		GOARCH string `json:"goarch"`
	}](t, readFile(t, filepath.Join(src, "dist", "artifacts.json")))

	for _, a := range artifacts {
		if a.Type == "Binary" && a.GOOS == goos && a.GOARCH == goarch {
			return filepath.Join(src, filepath.FromSlash(a.Path))
		}
	}

	t.Fatalf("dist/artifacts.json has no binary for %s", platform)

	return ""
}

// releaseNpmEnv is the environment of npm in the containers that pack,
// publish and install; the npmrc directory is mounted at /e2e/npmrc.
func releaseNpmEnv() []string {
	return append(releaseRunEnv(), "NPM_CONFIG_USERCONFIG=/e2e/npmrc/npmrc")
}

// releaseRunEnv is the environment of every run of the launcher and of the
// native binary it is compared with.
func releaseRunEnv() []string {
	return []string{"NPM_CONFIG_UPDATE_NOTIFIER=false", "NPM_CONFIG_FUND=false", "NPM_CONFIG_AUDIT=false"}
}

// releaseNpmPublish publishes the tarballs of `packages build` to verdaccio
// as the release workflow's publish-npm job does: each tarball
// `packages publish-plan` prints, in that order, with npm of the Node image.
// The first run stops after two packages, like a job that failed half way;
// the next plan must list exactly the rest, launcher last, and the plan after
// that nothing. A plan for a tarball of a published version with other bytes
// must fail.
func releaseNpmPublish(t *testing.T, src, packages, npmrc, work, base, version string) {
	t.Helper()

	checksums := "schepherd_" + version + "_npm_checksums.txt"

	var order []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(readFile(t, filepath.Join(packages, checksums)))), "\n") {
		_, file, _ := strings.Cut(line, "  ")
		order = append(order, file)
	}

	if want := releaseTarballOrder(version); !slices.Equal(order, want) {
		t.Fatalf("packages build recorded %v, want %v", order, want)
	}

	plan := func(dir, sums string) result {
		t.Helper()

		return hostTool(t, runOpts{Dir: src, Env: append(releaseGitEnv(), "GOWORK=off"), Timeout: 10 * time.Minute}, "go", "run", "./tools/release",
			"packages", "publish-plan", "--kind", "npm", "--dir", dir, "--checksums", sums, "--version", version, "--registry", base)
	}

	opts := dockerOpts{
		Network: composeNetwork(),
		Mounts:  []mount{{Host: filepath.Join(packages, "npm"), Container: "/e2e/dist"}, {Host: npmrc, Container: "/e2e/npmrc"}},
		Env:     releaseNpmEnv(),
	}

	publish := func(files []string) {
		t.Helper()

		for _, file := range files {
			dockerRun(t, nodeImage, opts, "npm", "publish", "/e2e/dist/"+file, "--access", "public", "--ignore-scripts", "--tag", releaseNpmDistTag).ok(t)
		}
	}

	npmDir, sums := filepath.Join(packages, "npm"), filepath.Join(packages, checksums)

	first := strings.Fields(string(plan(npmDir, sums).ok(t).Stdout))
	if !slices.Equal(first, order) {
		t.Fatalf("the first plan publishes %v, want every tarball in the order of %s: %v", first, checksums, order)
	}

	publish(first[:2])

	resumed := plan(npmDir, sums).ok(t)
	if got := strings.Fields(string(resumed.Stdout)); !slices.Equal(got, order[2:]) {
		t.Fatalf("the plan after an interrupted publication publishes %v, want %v\n%s", got, order[2:], resumed)
	}

	for _, file := range first[:2] {
		if !bytes.Contains(resumed.Stderr, []byte("already published: "+file)) {
			t.Errorf("the resumed plan does not report %s as published:\n%s", file, resumed)
		}
	}

	publish(order[2:])

	if done := plan(npmDir, sums).ok(t); len(bytes.TrimSpace(done.Stdout)) != 0 {
		t.Fatalf("the plan after a complete publication still publishes:\n%s", done)
	}

	changed := releaseRepackedDist(t, npmDir, sums, work, order[0])
	if res := plan(changed, filepath.Join(changed, checksums)); res.Code != 1 || !bytes.Contains(res.Stderr, []byte(order[0]+" is already published with other contents")) {
		t.Fatalf("a plan for a tarball whose bytes differ from the published version:\n%s", res)
	}
}

// releaseTarballOrder is the order the tarballs are recorded and published
// in: the platform packages by name, the launcher package last.
func releaseTarballOrder(version string) []string {
	order := make([]string, 0, len(releaseNpmPackages))
	for _, dir := range releaseNpmPackages[1:] {
		order = append(order, "ovineko-"+dir+"-"+version+".tgz")
	}

	return append(order, "ovineko-schepherd-"+version+".tgz")
}

// releaseRepackedDist copies the tarballs and their checksum file into a new
// directory, compresses the tar stream of one tarball again so its bytes
// change while its package does not, and records the new SHA-256.
func releaseRepackedDist(t *testing.T, dist, sums, work, file string) string {
	t.Helper()

	changed := filepath.Join(work, "changed")

	var list strings.Builder

	for line := range strings.SplitSeq(strings.TrimSpace(string(readFile(t, sums))), "\n") {
		_, name, _ := strings.Cut(line, "  ")
		data := readFile(t, filepath.Join(dist, name))

		if name == file {
			gz, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}

			raw, err := io.ReadAll(gz)
			if err != nil {
				t.Fatal(err)
			}

			var buf bytes.Buffer

			w, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
			if err != nil {
				t.Fatal(err)
			}

			w.Comment = "repacked"

			if _, err := w.Write(raw); err != nil {
				t.Fatal(err)
			}

			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			data = buf.Bytes()
		}

		writeFile(t, filepath.Join(changed, name), data)
		list.WriteString(sha256Hex(data) + "  " + name + "\n")
	}

	writeFile(t, filepath.Join(changed, filepath.Base(sums)), []byte(list.String()))

	return changed
}

// releaseNpmDistTag is the dist-tag of the snapshot packages: a snapshot
// version is a pre-release, which the release workflow publishes under next,
// and npm refuses to publish a pre-release without --tag.
const releaseNpmDistTag = "next"

// releaseCheckNpmRegistry reads every package document from verdaccio on the
// host: each package has the version under the dist-tag next, and the
// launcher package depends optionally on exactly the six platform packages
// at that version.
func releaseCheckNpmRegistry(t *testing.T, base, version string) {
	t.Helper()

	type packument struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			OptionalDependencies map[string]string `json:"optionalDependencies"`
		} `json:"versions"`
	}

	for _, dir := range releaseNpmPackages {
		name := "@ovineko/" + dir

		resp, err := http.Get(base + "/@ovineko%2f" + dir)
		if err != nil {
			t.Fatal(err)
		}

		var doc packument

		err = json.NewDecoder(resp.Body).Decode(&doc)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK || err != nil {
			t.Fatalf("%s from verdaccio: status %d, %v", name, resp.StatusCode, err)
		}

		v, ok := doc.Versions[version]
		if !ok || doc.DistTags[releaseNpmDistTag] != version {
			t.Fatalf("%s: dist-tags %v, versions %v, want %s as %s", name, doc.DistTags, doc.Versions, version, releaseNpmDistTag)
		}

		if dir != "schepherd" {
			continue
		}

		deps := map[string]string{}
		for _, p := range releaseNpmPackages[1:] {
			deps["@ovineko/"+p] = version
		}

		if !maps.Equal(v.OptionalDependencies, deps) {
			t.Fatalf("%s optionalDependencies %v, want %v", name, v.OptionalDependencies, deps)
		}
	}
}
