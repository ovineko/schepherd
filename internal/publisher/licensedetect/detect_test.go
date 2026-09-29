package licensedetect

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/httpfetch"
	"github.com/ovineko/schepherd/internal/publisher/policy"
)

const (
	commit    = "0123456789abcdef0123456789abcdef01234567"
	mitText   = "MIT License\n\nCopyright (c) 2026 Example Authors\n\nPermission is hereby granted...\n"
	apacheTxt = "Apache License\nVersion 2.0, January 2004\n"
	noticeTxt = "Example Project\nCopyright 2026 Example Authors\n"
)

type reply struct {
	header   map[string]string
	body     string
	redirect string
	status   int
}

// service is a fake of one service: replies by path (with query), hits and
// the request headers it saw.
type service struct {
	srv     *httptest.Server
	replies map[string]reply
	hits    map[string]int
	auth    []string
	accepts []string
	mu      sync.Mutex
}

func newService(t *testing.T) *service {
	t.Helper()

	s := &service{replies: map[string]reply{}, hits: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.EscapedPath()
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}

		s.mu.Lock()
		s.hits[key]++
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.accepts = append(s.accepts, r.Header.Get("Accept"))
		rep, ok := s.replies[key]
		s.mu.Unlock()

		switch {
		case !ok:
			http.NotFound(w, r)
		case rep.redirect != "":
			http.Redirect(w, r, rep.redirect, http.StatusFound)
		default:
			for k, v := range rep.header {
				w.Header().Set(k, v)
			}

			w.WriteHeader(max(rep.status, http.StatusOK))
			_, _ = w.Write([]byte(rep.body))
		}
	}))
	t.Cleanup(s.srv.Close)

	return s
}

func (s *service) set(key string, rep reply) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.replies[key] = rep
}

func (s *service) hitCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.hits[key]
}

func (s *service) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, hits := range s.hits {
		n += hits
	}

	return n
}

type fakes struct {
	github, registry, unpkg, jsdelivr *service
}

func newFakes(t *testing.T) *fakes {
	t.Helper()

	return &fakes{github: newService(t), registry: newService(t), unpkg: newService(t), jsdelivr: newService(t)}
}

func (f *fakes) config(extra ...*httptest.Server) Config {
	servers := make([]*httptest.Server, 0, 4+len(extra))
	servers = append(servers, f.github.srv, f.registry.srv, f.unpkg.srv, f.jsdelivr.srv)
	servers = append(servers, extra...)

	private := make([]string, 0, len(servers))
	for _, srv := range servers {
		private = append(private, srv.Listener.Addr().String())
	}

	return Config{
		Hosts: []string{policy.HostGitHubAPI, policy.HostNPMRegistry, policy.HostUnpkg, policy.HostJSDelivr},
		Endpoints: map[string]string{
			policy.HostGitHubAPI: f.github.srv.URL, policy.HostNPMRegistry: f.registry.srv.URL,
			policy.HostUnpkg: f.unpkg.srv.URL, policy.HostJSDelivr: f.jsdelivr.srv.URL,
		},
		Fetch: httpfetch.Policy{AllowHTTP: true, AllowPrivateHosts: private},
	}
}

func (f *fakes) detector(t *testing.T, extra ...*httptest.Server) *Detector {
	t.Helper()

	d, err := New(f.config(extra...))
	if err != nil {
		t.Fatal(err)
	}

	return d
}

func blobID(data string) string {
	h := sha1.New()
	_, _ = fmt.Fprintf(h, "blob %d\x00%s", len(data), data)

	return hex.EncodeToString(h.Sum(nil))
}

func blobFields(data string) map[string]any {
	encoded := base64.StdEncoding.EncodeToString([]byte(data))

	var wrapped strings.Builder

	for len(encoded) > 60 {
		wrapped.WriteString(encoded[:60] + "\n")
		encoded = encoded[60:]
	}

	wrapped.WriteString(encoded)

	return map[string]any{"sha": blobID(data), "size": len(data), "content": wrapped.String(), "encoding": "base64"}
}

func jsonReply(t *testing.T, v any) reply {
	t.Helper()

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return reply{body: string(data)}
}

func licenseReply(t *testing.T, path, spdx, text string) reply {
	t.Helper()

	doc := blobFields(text)
	doc["path"], doc["name"] = path, path

	if spdx != "" {
		doc["license"] = map[string]any{"key": strings.ToLower(spdx), "spdx_id": spdx}
	} else {
		doc["license"] = nil
	}

	return jsonReply(t, doc)
}

// repo serves owner/repo with its main branch at commit.
func (f *fakes) repo(t *testing.T, spdx, text string) {
	t.Helper()

	f.github.set("/repos/owner/repo/commits/main", reply{body: commit})
	f.github.set("/repos/owner/repo/license?ref="+commit, licenseReply(t, "LICENSE", spdx, text))
}

func (f *fakes) listing(t *testing.T, files map[string]string) {
	t.Helper()

	entries := make([]map[string]any, 0, 1+len(files))
	entries = append(entries, map[string]any{"type": "dir", "name": "src", "sha": blobID("dir")})

	for name, data := range files {
		entries = append(entries, map[string]any{"type": "file", "name": name, "sha": blobID(data), "size": len(data)})
		f.github.set("/repos/owner/repo/git/blobs/"+blobID(data), jsonReply(t, blobFields(data)))
	}

	f.github.set("/repos/owner/repo/contents/?ref="+commit, jsonReply(t, entries))
}

func detect(t *testing.T, d *Detector, url string) policy.Finding {
	t.Helper()

	f, err := d.Detect(t.Context(), url)
	if err != nil {
		t.Fatalf("Detect(%s): %v", url, err)
	}

	return f
}

const rawURL = "https://raw.githubusercontent.com/owner/repo/main/schemas/config.json"

func TestGitHubPermissive(t *testing.T) {
	f := newFakes(t)
	f.repo(t, "MIT", mitText)

	got := detect(t, f.detector(t), rawURL)
	want := policy.Finding{
		Source: "github:owner/repo@" + commit, License: "MIT", LicenseFile: "LICENSE", LicenseDigest: digest.FromBytes([]byte(mitText)),
		Notice: "License of the GitHub repository owner/repo at main (SPDX: MIT), file LICENSE:\n\n" + strings.TrimSuffix(mitText, "\n"),
	}

	if got != want {
		t.Fatalf("finding =\n%#v\nwant\n%#v", got, want)
	}

	if f.github.hitCount("/repos/owner/repo/contents/?ref="+commit) != 0 {
		t.Error("the NOTICE file was looked up for a license without NOTICE semantics")
	}

	f.github.mu.Lock()
	defer f.github.mu.Unlock()

	if f.github.accepts[0] != "application/vnd.github.sha" || f.github.accepts[1] != "application/vnd.github+json" {
		t.Errorf("Accept headers = %v", f.github.accepts)
	}
}

func TestGitHubURLForms(t *testing.T) {
	f := newFakes(t)
	f.repo(t, "MIT", mitText)
	f.github.set("/repos/owner/repo/commits/heads/main", reply{body: commit})
	f.github.set("/repos/owner/repo/commits/tags/v1.2", reply{body: commit})

	d := f.detector(t)

	for url, notice := range map[string]string{
		"https://github.com/owner/repo/raw/main/schema.json":                          "at main (SPDX",
		"https://github.com/owner/repo/blob/" + commit + "/dir/schema.json":           "at " + commit + " (SPDX",
		"https://raw.githubusercontent.com/Owner/Repo/refs/heads/main/schema.json":    "at main (SPDX",
		"http://raw.githubusercontent.com/owner/repo/refs/tags/v1.2/a%20b.json#/x":    "at v1.2 (SPDX",
		"https://raw.githubusercontent.com:443/owner/repo/" + commit + "/schema.json": "at " + commit + " (SPDX",
	} {
		got := detect(t, d, url)
		if got.Failure != "" || got.Source != "github:owner/repo@"+commit || !strings.Contains(got.Notice, notice) {
			t.Errorf("Detect(%s) = %#v", url, got)
		}
	}

	if hits := f.github.hitCount("/repos/owner/repo/license?ref=" + commit); hits != 1 {
		t.Errorf("the license at one commit was requested %d times", hits)
	}

	if hits := f.github.hitCount("/repos/owner/repo/commits/main") + f.github.hitCount("/repos/owner/repo/commits/heads/main"); hits != 1 {
		t.Errorf("main and refs/heads/main were resolved %d times, want once", hits)
	}
}

func TestQualifiedRefSharesTheFinding(t *testing.T) {
	f := newFakes(t)
	f.github.set("/repos/owner/repo/commits/heads/main", reply{body: commit})
	f.github.set("/repos/owner/repo/license?ref="+commit, licenseReply(t, "LICENSE", "MIT", mitText))

	d := f.detector(t)
	first := detect(t, d, "https://github.com/owner/repo/raw/refs/heads/main/schema.json")

	// GitHub redirects /raw/ URLs to raw.githubusercontent.com; the target
	// must be vetted from the cache before it is contacted.
	got, ok := d.Cached(rawURL)
	if !ok || got != first || first.Failure != "" || !strings.Contains(first.Notice, "owner/repo at main (SPDX") {
		t.Fatalf("Cached(%s) = %#v, %v; first finding %#v", rawURL, got, ok, first)
	}
}

func TestGitHubApacheNotice(t *testing.T) {
	t.Run("with NOTICE", func(t *testing.T) {
		f := newFakes(t)
		f.repo(t, "Apache-2.0", apacheTxt)
		f.listing(t, map[string]string{"README.md": "readme", "notice.txt": "other", "NOTICE": noticeTxt})

		got := detect(t, f.detector(t), rawURL)
		wantNotice := "License of the GitHub repository owner/repo at main (SPDX: Apache-2.0), file LICENSE:\n\n" +
			"Apache License\nVersion 2.0, January 2004\n\nNOTICE of the GitHub repository owner/repo at main, file NOTICE:\n\n" +
			"Example Project\nCopyright 2026 Example Authors"

		if got.Notice != wantNotice || got.NoticeFile != "NOTICE" || got.NoticeDigest != digest.FromBytes([]byte(noticeTxt)) {
			t.Fatalf("finding = %#v", got)
		}
	})

	t.Run("without NOTICE", func(t *testing.T) {
		f := newFakes(t)
		f.repo(t, "Apache-2.0", apacheTxt)
		f.listing(t, map[string]string{"README.md": "readme"})

		got := detect(t, f.detector(t), rawURL)
		if got.Failure != "" || got.NoticeFile != "" || strings.Contains(got.Notice, "NOTICE") || !strings.HasSuffix(got.Notice, strings.TrimSuffix(apacheTxt, "\n")) {
			t.Fatalf("finding = %#v", got)
		}

		if f.github.hitCount("/repos/owner/repo/git/blobs/"+blobID("readme")) != 0 {
			t.Error("a file other than NOTICE was downloaded")
		}
	})

	t.Run("NOTICE is a symbolic link", func(t *testing.T) {
		f := newFakes(t)
		f.repo(t, "Apache-2.0", apacheTxt)
		f.github.set("/repos/owner/repo/contents/?ref="+commit, jsonReply(t, []map[string]any{
			{"type": "symlink", "name": "NOTICE", "sha": blobID("NOTICE.md")},
		}))

		got := detect(t, f.detector(t), rawURL)
		if got.Failure != policy.RefusedFetchFailed || !strings.Contains(got.Detail, `"NOTICE" is a symlink`) || got.Notice != "" {
			t.Fatalf("finding = %#v", got)
		}
	})

	t.Run("a notice directory is not a NOTICE file", func(t *testing.T) {
		f := newFakes(t)
		f.repo(t, "Apache-2.0", apacheTxt)
		f.github.set("/repos/owner/repo/contents/?ref="+commit, jsonReply(t, []map[string]any{
			{"type": "dir", "name": "notice", "sha": blobID("dir")},
		}))

		if got := detect(t, f.detector(t), rawURL); got.Failure != "" || got.NoticeFile != "" {
			t.Fatalf("finding = %#v", got)
		}
	})

	t.Run("NOTICE blob does not match the listing", func(t *testing.T) {
		f := newFakes(t)
		f.repo(t, "Apache-2.0", apacheTxt)
		f.listing(t, map[string]string{"NOTICE": noticeTxt})

		tampered := blobFields(noticeTxt)
		tampered["content"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", len(noticeTxt))))
		f.github.set("/repos/owner/repo/git/blobs/"+blobID(noticeTxt), jsonReply(t, tampered))

		got := detect(t, f.detector(t), rawURL)
		if got.Failure != policy.RefusedFetchFailed || !strings.Contains(got.Detail, "blob ID") {
			t.Fatalf("finding = %#v", got)
		}
	})
}

func TestGitHubLicenseAnswers(t *testing.T) {
	t.Run("NOASSERTION", func(t *testing.T) {
		f := newFakes(t)
		f.repo(t, "NOASSERTION", "Custom terms\n")

		if got := detect(t, f.detector(t), rawURL); got.License != "NOASSERTION" || got.Failure != "" {
			t.Fatalf("finding = %#v", got)
		}
	})

	t.Run("unrecognized license", func(t *testing.T) {
		f := newFakes(t)
		f.repo(t, "", "Custom terms\n")

		if got := detect(t, f.detector(t), rawURL); got.License != "NOASSERTION" {
			t.Fatalf("finding = %#v", got)
		}
	})

	t.Run("no license file", func(t *testing.T) {
		f := newFakes(t)
		f.github.set("/repos/owner/repo/commits/main", reply{body: commit})

		got := detect(t, f.detector(t), rawURL)
		if got.Failure != policy.RefusedNoLicense || got.Source != "github:owner/repo@"+commit || !strings.Contains(got.Detail, "no license file") {
			t.Fatalf("finding = %#v", got)
		}
	})

	t.Run("unknown ref", func(t *testing.T) {
		f := newFakes(t)

		got := detect(t, f.detector(t), rawURL)
		if got.Failure != policy.RefusedFetchFailed || !strings.Contains(got.Detail, `ref "main" does not exist`) {
			t.Fatalf("finding = %#v", got)
		}
	})

	t.Run("ref resolves to something else", func(t *testing.T) {
		f := newFakes(t)
		f.github.set("/repos/owner/repo/commits/main", reply{body: "<html>"})

		if got := detect(t, f.detector(t), rawURL); got.Failure != policy.RefusedFetchFailed {
			t.Fatalf("finding = %#v", got)
		}
	})

	t.Run("content does not match size", func(t *testing.T) {
		f := newFakes(t)
		f.repo(t, "MIT", mitText)

		doc := blobFields(mitText)
		doc["path"], doc["size"], doc["license"] = "LICENSE", 3, map[string]any{"spdx_id": "MIT"}
		f.github.set("/repos/owner/repo/license?ref="+commit, jsonReply(t, doc))

		if got := detect(t, f.detector(t), rawURL); got.Failure != policy.RefusedFetchFailed || !strings.Contains(got.Detail, "GitHub reports 3") {
			t.Fatalf("finding = %#v", got)
		}
	})

	t.Run("license path escapes the repository", func(t *testing.T) {
		f := newFakes(t)
		f.repo(t, "MIT", mitText)
		f.github.set("/repos/owner/repo/license?ref="+commit, licenseReply(t, "../LICENSE", "MIT", mitText))

		if got := detect(t, f.detector(t), rawURL); got.Failure != policy.RefusedFetchFailed || !strings.Contains(got.Detail, "clean relative path") {
			t.Fatalf("finding = %#v", got)
		}
	})

	t.Run("license text with control characters", func(t *testing.T) {
		f := newFakes(t)
		f.repo(t, "MIT", "MIT\x1b[31m License\n")

		got := detect(t, f.detector(t), rawURL)
		if got.Failure != policy.RefusedNoLicense || got.License != "MIT" || !strings.Contains(got.Detail, "control characters") || got.Notice != "" {
			t.Fatalf("finding = %#v", got)
		}
	})

	t.Run("license text with bidirectional controls", func(t *testing.T) {
		f := newFakes(t)
		f.repo(t, "MIT", "MIT \u202eesneciL\n")

		if got := detect(t, f.detector(t), rawURL); got.Failure != policy.RefusedNoLicense {
			t.Fatalf("finding = %#v", got)
		}
	})

	t.Run("CRLF and byte order mark", func(t *testing.T) {
		f := newFakes(t)
		f.repo(t, "MIT", "\ufeffMIT License\r\n\r\nText\r\n\r\n")

		got := detect(t, f.detector(t), rawURL)
		if !strings.HasSuffix(got.Notice, "file LICENSE:\n\nMIT License\n\nText") {
			t.Fatalf("notice = %q", got.Notice)
		}
	})
}

func TestNPMPermissive(t *testing.T) {
	f := newFakes(t)
	f.registry.set("/@scope%2fpkg/1.2.3", jsonReply(t, map[string]any{"name": "@scope/pkg", "version": "1.2.3", "license": "MIT"}))
	f.unpkg.set("/@scope/pkg@1.2.3/LICENSE.md", reply{body: mitText})

	got := detect(t, f.detector(t), "https://unpkg.com/@scope/pkg@1.2.3/schemas/config.json")
	want := policy.Finding{
		Source: "npm:@scope/pkg@1.2.3", License: "MIT", LicenseFile: "LICENSE.md", LicenseDigest: digest.FromBytes([]byte(mitText)),
		Notice: "License of the npm package @scope/pkg version 1.2.3 (SPDX: MIT), file LICENSE.md:\n\n" + strings.TrimSuffix(mitText, "\n"),
	}

	if got != want {
		t.Fatalf("finding =\n%#v\nwant\n%#v", got, want)
	}

	if f.unpkg.hitCount("/@scope/pkg@1.2.3/LICENSE") != 1 || f.unpkg.hitCount("/@scope/pkg@1.2.3/LICENSE.txt") != 0 {
		t.Errorf("license files probed: %v", f.unpkg.hits)
	}

	for _, auth := range append(f.registry.auth, f.unpkg.auth...) {
		if auth != "" {
			t.Errorf("an npm service received Authorization %q", auth)
		}
	}
}

func TestNPMExpressionsAndNotice(t *testing.T) {
	f := newFakes(t)
	f.registry.set("/dual/2.0.0-rc.1", jsonReply(t, map[string]any{"name": "dual", "version": "2.0.0-rc.1", "license": "(MIT OR Apache-2.0)"}))
	f.jsdelivr.set("/npm/dual@2.0.0-rc.1/LICENSE", reply{body: apacheTxt})
	f.jsdelivr.set("/npm/dual@2.0.0-rc.1/NOTICE.md", reply{body: noticeTxt})

	got := detect(t, f.detector(t), "https://cdn.jsdelivr.net/npm/dual@2.0.0-rc.1/schema.json")
	if got.License != "(MIT OR Apache-2.0)" || got.NoticeFile != "NOTICE.md" ||
		!strings.HasSuffix(got.Notice, "NOTICE of the npm package dual version 2.0.0-rc.1, file NOTICE.md:\n\nExample Project\nCopyright 2026 Example Authors") {
		t.Fatalf("finding = %#v", got)
	}
}

func TestNPMLicenseAnswers(t *testing.T) {
	const url = "https://unpkg.com/pkg@1.0.0/schema.json"

	cases := []struct {
		meta    map[string]any
		name    string
		license string
		failure string
		detail  string
		noFile  bool
	}{
		{name: "legacy object", meta: map[string]any{"license": map[string]any{"type": "ISC", "url": "https://example.com"}}, license: "ISC"},
		{name: "legacy single-entry array", meta: map[string]any{"licenses": []any{map[string]any{"type": "MIT"}}}, license: "MIT"},
		{name: "legacy array", meta: map[string]any{"licenses": []any{map[string]any{"type": "MIT"}, map[string]any{"type": "GPL-2.0"}}}, failure: policy.RefusedNotAsserted, detail: "deprecated licenses array"},
		{name: "no license", meta: map[string]any{}, failure: policy.RefusedNoLicense, detail: "declares no license"},
		{name: "null license", meta: map[string]any{"license": nil}, failure: policy.RefusedNoLicense},
		{name: "unlicensed", meta: map[string]any{"license": "UNLICENSED"}, license: "UNLICENSED"},
		{name: "see license in", meta: map[string]any{"license": "SEE LICENSE IN LICENSE.txt"}, license: "SEE LICENSE IN LICENSE.txt"},
		{name: "no license file", meta: map[string]any{"license": "MIT"}, noFile: true, failure: policy.RefusedNoLicense, detail: "has no license file on unpkg.com"},
		{name: "other package", meta: map[string]any{"name": "evil", "license": "MIT"}, failure: policy.RefusedFetchFailed, detail: "answered with evil@1.0.0"},
		{name: "other version", meta: map[string]any{"version": "1.0.1", "license": "MIT"}, failure: policy.RefusedFetchFailed, detail: "answered with pkg@1.0.1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakes(t)

			meta := map[string]any{"name": "pkg", "version": "1.0.0"}
			maps.Copy(meta, tc.meta)

			f.registry.set("/pkg/1.0.0", jsonReply(t, meta))

			if !tc.noFile {
				f.unpkg.set("/pkg@1.0.0/LICENSE", reply{body: mitText})
			}

			got := detect(t, f.detector(t), url)
			if got.Source != "npm:pkg@1.0.0" || got.Failure != tc.failure || !strings.Contains(got.Detail, tc.detail) ||
				(tc.failure == "" && got.License != tc.license) {
				t.Fatalf("finding = %#v", got)
			}
		})
	}

	t.Run("version missing from the registry", func(t *testing.T) {
		got := detect(t, newFakes(t).detector(t), url)
		if got.Failure != policy.RefusedFetchFailed || !strings.Contains(got.Detail, "the npm registry has no pkg@1.0.0") {
			t.Fatalf("finding = %#v", got)
		}
	})
}

func TestRecognize(t *testing.T) {
	f := newFakes(t)
	d := f.detector(t)

	unsupported := map[string]string{
		"https://example.com/schema.json":                                   "neither a GitHub nor a supported npm CDN host",
		"ftp://raw.githubusercontent.com/owner/repo/main/x.json":            "not an absolute http(s) URL",
		"https://raw.githubusercontent.com/owner/repo/main/x.json?token=1":  "has a query",
		"https://raw.githubusercontent.com:8443/owner/repo/main/x.json":     "names a port",
		"https://user@raw.githubusercontent.com/owner/repo/main/x.json":     "credentials",
		"https://raw.githubusercontent.com/owner/repo/main":                 "does not name a file",
		"https://raw.githubusercontent.com/owner/repo/main/":                "does not name a file",
		"https://raw.githubusercontent.com/owner/repo/main/../other/x.json": "dot segments",
		"https://raw.githubusercontent.com/owner%2Frepo/x/main/x.json":      "encoded separator",
		"https://raw.githubusercontent.com/-owner/repo/main/x.json":         "valid GitHub owner",
		"https://raw.githubusercontent.com/owner/repo/ma..in/x.json":        "cannot be pinned",
		"https://github.com/owner/repo/tree/main/x.json":                    "does not name a file",
		"https://unpkg.com/pkg/schema.json":                                 "no exact version",
		"https://unpkg.com/pkg@^1.0.0/schema.json":                          "no exact version",
		"https://unpkg.com/pkg@latest/schema.json":                          "no exact version",
		"https://unpkg.com/pkg@1.0.0+build.1/schema.json":                   "no exact version",
		"https://unpkg.com/pkg@1.2.3-01/schema.json":                        "no exact version",
		"https://unpkg.com/pkg@1.2/schema.json":                             "no exact version",
		"https://unpkg.com/pkg@v1.2.3/schema.json":                          "no exact version",
		"https://unpkg.com/pkg@01.2.3/schema.json":                          "no exact version",
		"https://unpkg.com/@scope/pkg@1.0.0":                                "does not name a file",
		"https://unpkg.com/Bad%20Name@1.0.0/x.json":                         "valid npm package",
		"https://cdn.jsdelivr.net/gh/owner/repo@main/x.json":                "only npm packages",
	}

	for url, want := range unsupported {
		got := detect(t, d, url)
		if got.Failure != policy.RefusedUnsupportedHost || !strings.Contains(got.Detail, want) {
			t.Errorf("Detect(%s) = %#v, want unsupported-host containing %q", url, got, want)
		}
	}

	if n := f.github.total() + f.registry.total() + f.unpkg.total() + f.jsdelivr.total(); n != 0 {
		t.Errorf("unsupported URLs caused %d requests", n)
	}

	cfg := f.config()
	cfg.Hosts = []string{policy.HostGitHubAPI, policy.HostNPMRegistry}

	limited, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if got := detect(t, limited, "https://unpkg.com/pkg@1.0.0/x.json"); got.Failure != policy.RefusedUnsupportedHost ||
		!strings.Contains(got.Detail, "needs unpkg.com, which the [auto] hosts do not list") {
		t.Errorf("host outside [auto] hosts = %#v", got)
	}
}

func TestRateLimit(t *testing.T) {
	f := newFakes(t)
	f.github.set("/repos/owner/repo/commits/main", reply{
		status: http.StatusForbidden, body: `{"message":"API rate limit exceeded"}`,
		header: map[string]string{"X-Ratelimit-Remaining": "0", "X-Ratelimit-Reset": "1790000000"},
	})

	var (
		logs []string
		mu   sync.Mutex
	)

	cfg := f.config()
	cfg.Log = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()

		logs = append(logs, fmt.Sprintf(format, args...))
	}

	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	got := detect(t, d, rawURL)
	if got.Failure != policy.RefusedFetchFailed ||
		!strings.Contains(got.Detail, "api.github.com rate limit exceeded; it resets at 2026-09-21T") ||
		!strings.Contains(got.Detail, "set GITHUB_TOKEN") {
		t.Fatalf("finding = %#v", got)
	}

	other := detect(t, d, "https://raw.githubusercontent.com/other/lib/v1/x.json")
	if other.Failure != policy.RefusedFetchFailed || !strings.Contains(other.Detail, "rate limit exceeded") {
		t.Fatalf("finding after the limit = %#v", other)
	}

	if n := f.github.total(); n != 1 {
		t.Errorf("GitHub received %d requests; after a rate limit it must receive none", n)
	}

	if len(logs) != 1 || !strings.Contains(logs[0], "no further requests") {
		t.Errorf("logs = %v", logs)
	}

	f.registry.set("/pkg/1.0.0", reply{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "120"}})

	if got := detect(t, d, "https://unpkg.com/pkg@1.0.0/x.json"); got.Failure != policy.RefusedFetchFailed ||
		!strings.Contains(got.Detail, `registry.npmjs.org rate limit exceeded; it asks to retry after "120"`) || strings.Contains(got.Detail, "GITHUB_TOKEN") {
		t.Fatalf("npm finding = %#v", got)
	}
}

func TestForbiddenWithoutRateLimitIsAPlainFailure(t *testing.T) {
	f := newFakes(t)
	f.github.set("/repos/owner/repo/commits/main", reply{status: http.StatusForbidden})
	f.github.set("/repos/other/lib/commits/main", reply{body: commit})
	f.github.set("/repos/other/lib/license?ref="+commit, licenseReply(t, "LICENSE", "MIT", mitText))

	d := f.detector(t)

	if got := detect(t, d, rawURL); got.Failure != policy.RefusedFetchFailed || strings.Contains(got.Detail, "rate limit") {
		t.Fatalf("finding = %#v", got)
	}

	if got := detect(t, d, "https://raw.githubusercontent.com/other/lib/main/x.json"); got.Failure != "" {
		t.Fatalf("a plain 403 stopped later requests: %#v", got)
	}
}

func TestTokenGoesToGitHubOnly(t *testing.T) {
	f := newFakes(t)
	f.repo(t, "MIT", mitText)
	f.registry.set("/pkg/1.0.0", jsonReply(t, map[string]any{"name": "pkg", "version": "1.0.0", "license": "MIT"}))
	f.unpkg.set("/pkg@1.0.0/LICENSE", reply{body: mitText})

	cfg := f.config()
	cfg.Token = "ghs_secret"

	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	detect(t, d, rawURL)
	detect(t, d, "https://unpkg.com/pkg@1.0.0/x.json")

	for _, auth := range f.github.auth {
		if auth != "Bearer ghs_secret" {
			t.Errorf("GitHub Authorization = %q", auth)
		}
	}

	for _, auth := range append(f.registry.auth, f.unpkg.auth...) {
		if auth != "" {
			t.Errorf("npm service Authorization = %q", auth)
		}
	}
}

func TestRedirectToForeignHostRefused(t *testing.T) {
	foreign := newService(t)
	foreign.set("/license", licenseReply(t, "LICENSE", "MIT", mitText))

	f := newFakes(t)
	f.github.set("/repos/owner/repo/commits/main", reply{body: commit})
	f.github.set("/repos/owner/repo/license?ref="+commit, reply{redirect: foreign.srv.URL + "/license"})

	got := detect(t, f.detector(t, foreign.srv), rawURL)
	if got.Failure != policy.RefusedFetchFailed || !strings.Contains(got.Detail, "not a license detection service") {
		t.Fatalf("finding = %#v", got)
	}

	if foreign.total() != 0 {
		t.Error("the foreign redirect target was contacted")
	}
}

func TestRedirectToServiceOutsideHostsRefused(t *testing.T) {
	f := newFakes(t)
	f.registry.set("/license", licenseReply(t, "LICENSE", "MIT", mitText))
	f.github.set("/repos/owner/repo/commits/main", reply{body: commit})
	f.github.set("/repos/owner/repo/license?ref="+commit, reply{redirect: f.registry.srv.URL + "/license"})

	cfg := f.config()
	cfg.Hosts = []string{policy.HostGitHubAPI}

	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if got := detect(t, d, rawURL); got.Failure != policy.RefusedFetchFailed || !strings.Contains(got.Detail, "not a license detection service") {
		t.Fatalf("finding = %#v", got)
	}

	if f.registry.total() != 0 {
		t.Error("a service the [auto] hosts do not list was contacted through a redirect")
	}
}

func TestRedirectWithinServiceFollowed(t *testing.T) {
	f := newFakes(t)
	f.github.set("/repos/owner/repo/commits/main", reply{redirect: "/repositories/42/commits/main"})
	f.github.set("/repositories/42/commits/main", reply{body: commit})
	f.github.set("/repos/owner/repo/license?ref="+commit, licenseReply(t, "LICENSE", "MIT", mitText))

	if got := detect(t, f.detector(t), rawURL); got.Failure != "" || got.License != "MIT" {
		t.Fatalf("finding = %#v", got)
	}
}

func TestRenamedBranchIsResolved(t *testing.T) {
	for name, missing := range map[string]reply{
		"422": {status: http.StatusUnprocessableEntity, body: `{"message":"No commit found for SHA: main"}`},
		"404": {status: http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakes(t)
			f.github.set("/repos/owner/repo/commits/main", missing)
			f.github.set("/repos/owner/repo/branches/main", reply{redirect: "/repos/owner/repo/branches/trunk"})
			f.github.set("/repos/owner/repo/branches/trunk", jsonReply(t, map[string]any{"name": "trunk", "commit": map[string]any{"sha": commit}}))
			f.github.set("/repos/owner/repo/license?ref="+commit, licenseReply(t, "LICENSE", "MIT", mitText))

			got := detect(t, f.detector(t), rawURL)
			if got.Failure != "" || got.License != "MIT" || got.Source != "github:owner/repo@"+commit {
				t.Fatalf("finding = %#v", got)
			}
		})
	}
}

func TestMissingRefNamesTheMissingRef(t *testing.T) {
	f := newFakes(t)
	f.github.set("/repos/owner/repo/commits/main", reply{status: http.StatusUnprocessableEntity})

	got := detect(t, f.detector(t), rawURL)
	if got.Failure != policy.RefusedFetchFailed || !strings.Contains(got.Detail, "does not exist") {
		t.Fatalf("finding = %#v", got)
	}

	if hits := f.github.hitCount("/repos/owner/repo/branches/main"); hits != 1 {
		t.Errorf("branches endpoint asked %d times, want once", hits)
	}
}

func TestRejectedTokenStopsTheRun(t *testing.T) {
	f := newFakes(t)
	f.github.set("/repos/owner/repo/commits/main", reply{status: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`})

	cfg := f.config()
	cfg.Token = "ghp_rejected"

	det, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	_, err = det.Detect(t.Context(), rawURL)
	if fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "GITHUB_TOKEN") || strings.Contains(err.Error(), "ghp_rejected") {
		t.Fatalf("Detect = %v, want a usage error naming GITHUB_TOKEN without its value", err)
	}
}

func TestFetchFailure(t *testing.T) {
	f := newFakes(t)
	f.github.set("/repos/owner/repo/commits/main", reply{body: commit})
	f.github.set("/repos/owner/repo/license?ref="+commit, reply{status: http.StatusInternalServerError})

	got := detect(t, f.detector(t), rawURL)
	if got.Failure != policy.RefusedFetchFailed || got.Source != "github:owner/repo@"+commit || !strings.Contains(got.Detail, "read the license: ") ||
		!strings.Contains(got.Detail, "500") {
		t.Fatalf("finding = %#v", got)
	}
}

func TestRequestsAreDeduplicated(t *testing.T) {
	f := newFakes(t)
	f.repo(t, "Apache-2.0", apacheTxt)
	f.listing(t, map[string]string{"NOTICE": noticeTxt})
	f.registry.set("/pkg/1.0.0", jsonReply(t, map[string]any{"name": "pkg", "version": "1.0.0", "license": "MIT"}))
	f.unpkg.set("/pkg@1.0.0/LICENSE", reply{body: mitText})
	f.jsdelivr.set("/npm/pkg@1.0.0/LICENSE", reply{body: mitText})

	cfg := f.config()
	cfg.Jobs = 2

	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	findings := make([]policy.Finding, 40)

	for i := range findings {
		url := fmt.Sprintf("https://raw.githubusercontent.com/owner/repo/main/schemas/%d.json", i)

		switch i % 4 {
		case 1:
			url = fmt.Sprintf("https://github.com/Owner/Repo/raw/main/%d.json", i)
		case 2:
			url = fmt.Sprintf("https://unpkg.com/pkg@1.0.0/%d.json", i)
		case 3:
			url = fmt.Sprintf("https://cdn.jsdelivr.net/npm/pkg@1.0.0/%d.json", i)
		}

		wg.Go(func() {
			found, err := d.Detect(t.Context(), url)
			if err != nil {
				t.Error(err)
			}

			findings[i] = found
		})
	}

	wg.Wait()

	for i, found := range findings {
		// i%4 is 0 or 1 for GitHub URLs, 2 or 3 for npm URLs; every URL of
		// one repository ref or package version gets the same finding.
		want := findings[0]
		if i%4 >= 2 {
			want = findings[2]
		}

		if found.Failure != "" || found.Notice == "" || found != want {
			t.Errorf("finding %d = %#v", i, found)
		}
	}

	for path, want := range map[string]int{
		"/repos/owner/repo/commits/main":                   1,
		"/repos/owner/repo/license?ref=" + commit:          1,
		"/repos/owner/repo/contents/?ref=" + commit:        1,
		"/repos/owner/repo/git/blobs/" + blobID(noticeTxt): 1,
	} {
		if got := f.github.hitCount(path); got != want {
			t.Errorf("%s requested %d times, want %d", path, got, want)
		}
	}

	if got := f.registry.hitCount("/pkg/1.0.0"); got != 1 {
		t.Errorf("registry metadata requested %d times across both CDNs", got)
	}

	if f.unpkg.hitCount("/pkg@1.0.0/LICENSE") != 1 || f.jsdelivr.hitCount("/npm/pkg@1.0.0/LICENSE") != 1 {
		t.Errorf("license files: unpkg %v, jsdelivr %v", f.unpkg.hits, f.jsdelivr.hits)
	}
}

func TestCached(t *testing.T) {
	f := newFakes(t)
	f.repo(t, "MIT", mitText)
	d := f.detector(t)

	if _, ok := d.Cached(rawURL); ok {
		t.Fatal("Cached has a finding before Detect")
	}

	if got, ok := d.Cached("https://example.com/x.json"); !ok || got.Failure != policy.RefusedUnsupportedHost {
		t.Fatalf("Cached(unsupported) = %#v, %v", got, ok)
	}

	want := detect(t, d, rawURL)

	got, ok := d.Cached("https://github.com/OWNER/repo/raw/main/other.json")
	if !ok || got != want {
		t.Fatalf("Cached for another file of the same repository ref = %#v, %v", got, ok)
	}

	if f.github.total() != 2 {
		t.Errorf("Cached made requests: %v", f.github.hits)
	}
}

func TestCanceled(t *testing.T) {
	f := newFakes(t)
	f.repo(t, "MIT", mitText)
	d := f.detector(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := d.Detect(ctx, rawURL)
	if fault.KindOf(err) != fault.Canceled {
		t.Fatalf("err = %v, want fault.Canceled", err)
	}

	if _, ok := d.Cached(rawURL); ok {
		t.Fatal("a canceled detection was remembered")
	}

	if got := detect(t, d, rawURL); got.Failure != "" || got.License != "MIT" {
		t.Fatalf("detection after a canceled one = %#v", got)
	}
}

func TestNewRejectsBadEndpoints(t *testing.T) {
	for _, endpoint := range []string{"ftp://x", "https://x/path", "https://user@x", "https://x?q=1", "::"} {
		_, err := New(Config{Endpoints: map[string]string{policy.HostGitHubAPI: endpoint}})
		if fault.KindOf(err) != fault.Usage {
			t.Errorf("New with endpoint %q: %v", endpoint, err)
		}
	}
}

// npm versions follow SemVer 2.0.0 only; Schepherd's own alpha/beta/rc
// release rules do not apply to them.
func TestExactVersion(t *testing.T) {
	for version, want := range map[string]bool{
		"1.2.3": true, "0.0.0": true, "1.2.3-next.20260101": true, "1.2.3-0": true, "1.2.3-x-y.1": true,
		"1.2.3-01": false, "1.2": false, "1": false, "v1.2.3": false, "1.2.3+build": false, "01.2.3": false, "": false,
	} {
		if got := exactVersion(version); got != want {
			t.Errorf("exactVersion(%q) = %v, want %v", version, got, want)
		}
	}
}
