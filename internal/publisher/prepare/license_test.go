package prepare

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/httpfetch"
	"github.com/ovineko/schepherd/internal/publisher/licensedetect"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/upstream"
)

const (
	detectedCommit = "89abcdef0123456789abcdef0123456789abcdef"
	mitLicense     = "MIT License\n\nCopyright (c) 2026 Example Authors\n"
	gplLicense     = "GNU GENERAL PUBLIC LICENSE\nVersion 3\n"
	mitURL         = "https://raw.githubusercontent.com/owner/mit/main/schema.json"
	gplURL         = "https://raw.githubusercontent.com/owner/gpl/main/schema.json"
	npmURL         = "https://unpkg.com/pkg@1.0.0/schema.json"
	heldURL        = "https://raw.githubusercontent.com/owner/held/main/schema.json"
	autoPolicyTOML = `
[auto]
enabled = true

[[rules]]
id = "store"
decision = "allow"
hosts = ["www.schemastore.org"]
license = "Apache-2.0"
notice = "Store notice."
reason = "store"

[[rules]]
id = "held"
decision = "review"
hosts = ["raw.githubusercontent.com"]
path_prefix = "/owner/held/"
reason = "owner asked for time"
`
)

// licenseServices fakes the GitHub API, the npm registry and unpkg on one
// server: their paths do not overlap.
type licenseServices struct {
	srv   *httptest.Server
	paths map[string]string
	hits  map[string]int
	mu    sync.Mutex
}

func newLicenseServices(t *testing.T) *licenseServices {
	t.Helper()

	s := &licenseServices{paths: map[string]string{}, hits: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.EscapedPath()
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}

		s.mu.Lock()
		s.hits[key]++
		body, ok := s.paths[key]
		s.mu.Unlock()

		if !ok {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.srv.Close)

	for repo, license := range map[string][2]string{"mit": {"MIT", mitLicense}, "gpl": {"GPL-3.0-only", gplLicense}} {
		s.paths["/repos/owner/"+repo+"/commits/main"] = detectedCommit
		s.paths["/repos/owner/"+repo+"/license?ref="+detectedCommit] = githubLicense(t, license[0], license[1])
	}

	s.paths["/pkg/1.0.0"] = `{"name":"pkg","version":"1.0.0","license":"ISC"}`
	s.paths["/pkg@1.0.0/LICENSE"] = "ISC License\n"

	return s
}

func githubLicense(t *testing.T, spdx, text string) string {
	t.Helper()

	h := sha1.New()
	_, _ = fmt.Fprintf(h, "blob %d\x00%s", len(text), text)

	data, err := json.Marshal(map[string]any{
		"path": "LICENSE", "sha": hex.EncodeToString(h.Sum(nil)), "size": len(text), "encoding": "base64",
		"content": base64.StdEncoding.EncodeToString([]byte(text)), "license": map[string]string{"spdx_id": spdx},
	})
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func (s *licenseServices) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, hits := range s.hits {
		n += hits
	}

	return n
}

func (s *licenseServices) detector(t *testing.T) *licensedetect.Detector {
	t.Helper()

	d, err := licensedetect.New(licensedetect.Config{
		Hosts: []string{policy.HostGitHubAPI, policy.HostNPMRegistry, policy.HostUnpkg},
		Endpoints: map[string]string{
			policy.HostGitHubAPI: s.srv.URL, policy.HostNPMRegistry: s.srv.URL, policy.HostUnpkg: s.srv.URL, policy.HostJSDelivr: s.srv.URL,
		},
		Fetch: httpfetch.Policy{AllowHTTP: true, AllowPrivateHosts: []string{s.srv.Listener.Addr().String()}},
	})
	if err != nil {
		t.Fatal(err)
	}

	return d
}

func detectingDecider(t *testing.T, content string, s *licenseServices) *decider {
	t.Helper()

	path := filepath.Join(t.TempDir(), "licenses.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	pol, err := policy.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	d := &decider{policy: pol}
	if err := d.enableDetection(s.detector(t), 2, nil); err != nil {
		t.Fatal(err)
	}

	return d
}

func decideWithDetection(t *testing.T, d *decider, root string, deps ...string) policy.Decision {
	t.Helper()

	decision, err := d.decideContext(t.Context(), root, deps)
	if err != nil {
		t.Fatal(err)
	}

	return decision
}

func TestDetectedLicenseAllows(t *testing.T) {
	s := newLicenseServices(t)
	d := detectingDecider(t, autoPolicyTOML, s)

	got := decideWithDetection(t, d, mitURL)
	want := policy.Decision{
		Decision: policy.Allow, License: "MIT",
		Notice: "License of the GitHub repository owner/mit at main (SPDX: MIT), file LICENSE:\n\n" +
			"MIT License\n\nCopyright (c) 2026 Example Authors",
		Reason: "license MIT detected in github:owner/mit@" + detectedCommit + " (automatic)",
		Detections: []policy.Detection{{
			URL: mitURL, Source: "github:owner/mit@" + detectedCommit, License: "MIT", LicenseFile: "LICENSE",
			LicenseDigest: sha(mitLicense), Verdict: policy.Allow,
		}},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decision =\n%#v\nwant\n%#v", got, want)
	}

	layer := (&preparer{}).notice(got, false)
	if string(layer) != want.Notice+"\n" {
		t.Fatalf("notice layer = %q", layer)
	}

	sibling := decideWithDetection(t, d, "https://github.com/Owner/MIT/raw/main/other/schema.json")
	if other := (&preparer{}).notice(sibling, false); NoticePathFor(other) != NoticePathFor(layer) {
		t.Fatalf("another schema of the same repository ref got another notice: %q", other)
	}

	combined := decideWithDetection(t, d, "https://www.schemastore.org/a.json", mitURL, npmURL, "http://json-schema.org/draft-07/schema#")
	if combined.Decision != policy.Allow || combined.RuleID != "store" || combined.License != "Apache-2.0 AND ISC AND MIT" ||
		!strings.HasPrefix(combined.Notice, "Store notice.\n\nLicense of ") || !strings.Contains(combined.Notice, "ISC License") ||
		len(combined.Detections) != 2 {
		t.Fatalf("combined decision = %#v", combined)
	}

	sources := licenseSources(&combined)
	if len(sources) != 2 || sources[0].URL != mitURL || sources[1].URL != npmURL || sources[1].Source != "npm:pkg@1.0.0" ||
		sources[1].LicenseFile != "LICENSE" || sources[1].LicenseDigest != sha("ISC License\n") {
		t.Fatalf("license sources = %#v", sources)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if hits := s.hits["/repos/owner/mit/license?ref="+detectedCommit]; hits != 1 {
		t.Errorf("the license of one repository ref was requested %d times", hits)
	}
}

func TestDetectionRefusals(t *testing.T) {
	s := newLicenseServices(t)
	d := detectingDecider(t, autoPolicyTOML, s)

	gpl := decideWithDetection(t, d, gplURL)
	if gpl.Decision != policy.Review || gpl.AutoReason != policy.RefusedNotPermissive ||
		len(gpl.Detections) != 1 || gpl.Detections[0].License != "GPL-3.0-only" || licenseSources(&gpl) != nil {
		t.Errorf("GPL source = %#v", gpl)
	}

	if got := decideWithDetection(t, d, mitURL, "https://example.com/dep.json"); got.Decision != policy.Review ||
		got.AutoReason != policy.RefusedUnsupportedHost || !strings.Contains(got.Reason, "dependency https://example.com/dep.json") {
		t.Errorf("unsupported dependency = %#v", got)
	}

	before := s.total()

	if got := decideWithDetection(t, d, heldURL); got.Decision != policy.Review || got.RuleID != "held" || got.AutoReason != "" {
		t.Errorf("held source = %#v", got)
	}

	if s.total() != before {
		t.Error("a URL an explicit rule decides was sent to detection")
	}
}

func TestDetectionNeedsTheAutoSection(t *testing.T) {
	s := newLicenseServices(t)
	d := detectingDecider(t, strings.Replace(autoPolicyTOML, "enabled = true", "enabled = false", 1), s)

	if d.detector != nil {
		t.Fatal("a detector was enabled for a policy with detection disabled")
	}

	if got := decideWithDetection(t, d, mitURL); !reflect.DeepEqual(got, policy.Decision{Decision: policy.Review, Reason: policy.NoRuleReason}) {
		t.Fatalf("decision = %#v", got)
	}

	if s.total() != 0 {
		t.Fatalf("detection made %d requests", s.total())
	}
}

func TestDeclaredLicenseBeatsDetection(t *testing.T) {
	s := newLicenseServices(t)
	d := detectingDecider(t, autoPolicyTOML, s)
	d.declared = map[string]string{normalizeURI(gplURL): "LicenseRef-Reviewed"}

	got := decideWithDetection(t, d, gplURL, mitURL)
	if got.Decision != policy.Allow || got.License != "LicenseRef-Reviewed AND MIT" || len(got.Detections) != 1 ||
		got.Detections[0].URL != mitURL {
		t.Fatalf("decision = %#v", got)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.hits["/repos/owner/gpl/commits/main"] != 0 {
		t.Error("a URL with a declared license was sent to detection")
	}
}

func TestRedirectWithinDetectedSource(t *testing.T) {
	s := newLicenseServices(t)
	d := detectingDecider(t, autoPolicyTOML, s)

	if _, err := d.singleContext(t.Context(), "https://github.com/owner/mit/raw/main/schema.json"); err != nil {
		t.Fatal(err)
	}

	before := s.total()

	if got := d.single(mitURL); got.Decision != policy.Allow || got.License != "MIT" {
		t.Fatalf("redirect target in the detected repository ref = %#v", got)
	}

	if got := d.single("https://raw.githubusercontent.com/owner/other/main/x.json"); got.Decision != policy.Review || got.AutoReason != "" {
		t.Fatalf("undetected redirect target = %#v", got)
	}

	if s.total() != before {
		t.Error("vetting a redirect target made requests")
	}
}

func TestDetectionCanceled(t *testing.T) {
	d := detectingDecider(t, autoPolicyTOML, newLicenseServices(t))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := d.decideContext(ctx, mitURL, nil); fault.KindOf(err) != fault.Canceled {
		t.Fatalf("err = %v", err)
	}
}

func TestDetectionWaitsOutGitHubLimitsOnlyWithAToken(t *testing.T) {
	auto := policy.Auto{Enabled: true, Hosts: []string{policy.HostGitHubAPI}}

	withToken := detectionConfig(auto, "ghs_token", 3, nil)
	if withToken.RateLimitWait != authenticatedRateLimitWait || withToken.RateLimitWait < time.Hour ||
		withToken.Token != "ghs_token" || withToken.Jobs != 3 || !slices.Equal(withToken.Hosts, auto.Hosts) {
		t.Errorf("with a token: %+v", withToken)
	}

	if anonymous := detectionConfig(auto, "", 3, nil); anonymous.RateLimitWait != anonymousRateLimitWait ||
		anonymous.RateLimitWait >= time.Hour {
		t.Errorf("without a token: %+v", anonymous)
	}
}

func TestEnableDetectionFromEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "licenses.toml")
	if err := os.WriteFile(path, []byte(autoPolicyTOML), 0o600); err != nil {
		t.Fatal(err)
	}

	pol, err := policy.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv(env.KeyGitHubToken, "bad token")

	if err := (&decider{policy: pol}).enableDetection(nil, 1, nil); fault.KindOf(err) != fault.Usage {
		t.Fatalf("enableDetection with a broken token: %v", err)
	}

	t.Setenv(env.KeyGitHubToken, "ghs_token")

	d := &decider{policy: pol}
	if err := d.enableDetection(nil, 1, (&logRecorder{}).logf); err != nil || d.detector == nil {
		t.Fatalf("enableDetection = %v, detector %v", err, d.detector)
	}

	if err := (&decider{}).enableDetection(nil, 1, nil); err != nil {
		t.Fatalf("enableDetection without a policy: %v", err)
	}
}

func TestUpstreamNoticeCarriesTheLicense(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"snapshot.json":             `{"commit":"` + strings.Repeat("a", 40) + `","tarballDigest":"` + sha("tarball") + `","formatVersion":1}`,
		"src/api/json/catalog.json": `{"schemas":[]}`,
		"src/schemas/json/a.json":   `{}`,
		"LICENSE":                   "Apache License\r\nVersion 2.0\r\n",
		"NOTICE":                    "Store\nCopyright\n\n",
	})

	snap, err := upstream.OpenSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}

	if got := upstreamNotice(snap); got != "Apache License\r\nVersion 2.0\n\nStore\nCopyright" {
		t.Fatalf("upstreamNotice = %q", got)
	}

	if err := os.Remove(filepath.Join(dir, "NOTICE")); err != nil {
		t.Fatal(err)
	}

	if snap, err = upstream.OpenSnapshot(dir); err != nil {
		t.Fatal(err)
	}

	if got := upstreamNotice(snap); got != "Apache License\r\nVersion 2.0" {
		t.Fatalf("upstreamNotice without NOTICE = %q", got)
	}
}

func TestLicenseTotals(t *testing.T) {
	var totals LicenseTotals

	allowed := policy.Decision{Decision: policy.Allow, Detections: []policy.Detection{{Verdict: policy.Allow}}}
	totals.count(StatusIncluded, &allowed)
	totals.count(StatusIncluded, &policy.Decision{Decision: policy.Allow})

	for _, reason := range []string{
		policy.RefusedNoLicense, policy.RefusedNotPermissive, policy.RefusedNotPermissive, policy.RefusedNotAsserted,
		policy.RefusedUnsupportedHost, policy.RefusedFetchFailed,
	} {
		totals.count(StatusPendingReview, &policy.Decision{Decision: policy.Review, AutoReason: reason})
	}

	totals.count(StatusPendingReview, &policy.Decision{Decision: policy.Review, RuleID: "held"})
	totals.count(StatusExcluded, &policy.Decision{Decision: policy.Exclude, AutoReason: policy.RefusedFetchFailed})
	totals.count(StatusFailed, &allowed)

	want := LicenseTotals{AutoAllowed: 1, AutoRefused: AutoRefusals{
		NoLicense: 1, NotPermissive: 2, NotAsserted: 1, UnsupportedHost: 1, FetchFailed: 1,
	}}
	if totals != want || totals.Refused() != 6 {
		t.Fatalf("totals = %+v", totals)
	}

	data, err := json.Marshal(totals)
	if err != nil {
		t.Fatal(err)
	}

	if got := string(data); got != `{"autoAllowed":1,"autoRefused":{"noLicense":1,"notPermissive":2,"notAsserted":1,"unsupportedHost":1,"fetchFailed":1}}` {
		t.Fatalf("JSON = %s", got)
	}
}

// TestRunDetectsLicenses prepares a local source whose entries declare no
// license, so only automatic detection can allow them: the permissive one
// is included with the license text in its notice layer, the other one is
// held for review, the report records both detections and the license
// decision of the included entry pins the allowing one.
func TestRunDetectsLicenses(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"licenses.toml": "[auto]\nenabled = true\n",
		"source.toml": `kind = "local"
name = "detected"
policy = "licenses.toml"

[[entries]]
name = "MIT schema"
url = "` + mitURL + `"
file = "schemas/mit.json"
file_match = ["mit.json"]

[[entries]]
name = "GPL schema"
url = "` + gplURL + `"
file = "schemas/gpl.json"
file_match = ["gpl.json"]
`,
		"schemas/mit.json": `{"type":"object"}`,
		"schemas/gpl.json": `{"type":"string"}`,
	})

	services := newLicenseServices(t)
	logs := &logRecorder{}
	out := filepath.Join(t.TempDir(), "prepared")

	res, err := Run(t.Context(), Options{
		Tool:            pinnedTool(t),
		SourceFile:      filepath.Join(dir, "source.toml"),
		OutDir:          out,
		LicenseDetector: services.detector(t),
		Log:             logs.logf,
	})
	if err != nil {
		t.Fatal(err)
	}

	want := LicenseTotals{AutoAllowed: 1, AutoRefused: AutoRefusals{NotPermissive: 1}}
	if res.Totals.LicenseDetection != want || res.Totals.Included != 1 || res.Totals.PendingReview != 1 {
		t.Fatalf("totals = %+v", res.Totals)
	}

	logs.find(t, "license detection: 1 record(s) auto-allowed, 1 held for review", "1 not-permissive")

	set := mustLoad(t, out)
	if len(set.Document.Entries) != 1 {
		t.Fatalf("entries = %+v", set.Document.Entries)
	}

	entry := set.Document.Entries[0]
	wantNotice := "License of the GitHub repository owner/mit at main (SPDX: MIT), file LICENSE:\n\n" + strings.TrimRight(mitLicense, "\n") + "\n"

	if entry.Provenance.License != "MIT" || entry.Notice == "" || string(set.Notices[entry.Notice]) != wantNotice {
		t.Fatalf("entry %+v with notice %q, want license MIT and notice %q", entry, set.Notices[entry.Notice], wantNotice)
	}

	report := readReport(t, out)

	mit := recordByURL(t, report, mitURL)
	if mit.Status != StatusIncluded || len(mit.LicenseDetections) != 1 || mit.LicenseDetections[0].Verdict != policy.Allow {
		t.Errorf("MIT record = %+v", mit)
	}

	gpl := recordByURL(t, report, gplURL)
	if gpl.Status != StatusPendingReview || len(gpl.LicenseDetections) != 1 ||
		gpl.LicenseDetections[0].Reason != policy.RefusedNotPermissive || gpl.LicenseDetections[0].License != "GPL-3.0-only" {
		t.Errorf("GPL record = %+v", gpl)
	}

	if len(entry.License.Detections) != 1 || entry.License.Detections[0].Source != "github:owner/mit@"+detectedCommit ||
		entry.License.Detections[0].LicenseDigest != sha(mitLicense) {
		t.Errorf("license decision = %+v", entry.License)
	}
}
