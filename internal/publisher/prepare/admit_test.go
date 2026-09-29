package prepare

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/artifact"
)

// oddLicenseURL is hosted in a repository whose license file name the
// publisher state cannot record (it is not printable ASCII).
const oddLicenseURL = "https://raw.githubusercontent.com/owner/odd/main/schema.json"

func oddLicense(t *testing.T) string {
	t.Helper()

	answer := githubLicense(t, "MIT", mitLicense)
	if !strings.Contains(answer, `"path":"LICENSE"`) {
		t.Fatalf("unexpected license answer %s", answer)
	}

	return strings.Replace(answer, `"path":"LICENSE"`, `"path":"LICENSE-Größe"`, 1)
}

// A record whose entry prepared.json would refuse fails on its own instead
// of failing the validation of the whole prepared set, and so the run.
func TestUnrecordableLicenseDecisionFailsOnlyItsRecord(t *testing.T) {
	services := newLicenseServices(t)
	services.paths["/repos/owner/odd/commits/main"] = detectedCommit
	services.paths["/repos/owner/odd/license?ref="+detectedCommit] = oddLicense(t)

	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"licenses.toml": "[auto]\nenabled = true\n",
		"source.toml": `kind = "local"
name = "odd"
policy = "licenses.toml"

[[entries]]
name = "MIT schema"
url = "` + mitURL + `"
file = "mit.json"

[[entries]]
name = "Odd schema"
url = "` + oddLicenseURL + `"
file = "odd.json"
`,
		"mit.json": `{"type":"object"}`,
		"odd.json": `{"type":"string"}`,
	})

	out := filepath.Join(t.TempDir(), "out")

	res, err := Run(t.Context(), Options{
		Tool: pinnedTool(t), SourceFile: filepath.Join(dir, "source.toml"), OutDir: out, LicenseDetector: services.detector(t),
	})
	if err != nil {
		t.Fatalf("one record stopped the run: %v", err)
	}

	if res.Totals.Included != 1 || res.Totals.Failed != 1 || len(res.Regressions) != 1 || res.Regressions[0].URL != oddLicenseURL {
		t.Errorf("totals %+v, regressions %+v", res.Totals, res.Regressions)
	}

	if odd := recordByURL(t, readReport(t, out), oddLicenseURL); odd.Status != StatusFailed || odd.Reason != ReasonInvalidMetadata ||
		!strings.Contains(odd.Detail, "licenseFile") {
		t.Errorf("odd record = %+v", odd)
	}

	if entries := mustLoad(t, out).Document.Entries; len(entries) != 1 || entries[0].Provenance.Source != mitURL {
		t.Errorf("entries = %+v", entries)
	}
}

// Notices above the client's artifact limit fail their records during
// preparation; publish would otherwise refuse the whole prepared set.
func TestNoticeAboveTheClientLimitFailsItsRecords(t *testing.T) {
	e := newSchemaStoreEnv(t)

	// The NOTICE alone stays within the limit; with the LICENSE and the
	// rule's notice the notice layer does not.
	const line = "Attribution line of an upstream NOTICE file.\n"

	tree := schemaStoreTree(e.depURL)
	tree["NOTICE"] = strings.Repeat(line, int(artifact.DefaultLimits().MaxNoticeBytes)/len(line))
	writeSnapshot(t, e.snapshot, tree)

	out := filepath.Join(t.TempDir(), "out")

	if _, err := Run(t.Context(), e.options(t, out)); err != nil {
		t.Fatal(err)
	}

	set := mustLoad(t, out)
	for _, entry := range set.Document.Entries {
		if entry.Notice != "" {
			t.Errorf("%s has a notice of %d bytes", entry.ID, len(set.Notices[entry.Notice]))
		}
	}

	if a := recordByURL(t, readReport(t, out), "https://www.schemastore.org/a.json"); a.Status != StatusFailed || a.Reason != ReasonTooLarge {
		t.Errorf("a.json record = %+v", a)
	}
}

// A prepared schema above the client's artifact limit fails its record.
func TestSchemaAboveTheClientLimitFailsItsRecord(t *testing.T) {
	limit := artifact.DefaultLimits().MaxSchemaBytes
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"source.toml": fmt.Sprintf(`kind = "local"
name = "big"

[dependencies]
max_document_bytes = %d
max_total_bytes = %d

[[entries]]
name = "Big schema"
url = "https://schemas.example/big.json"
file = "big.json"
license = "MIT"
`, 2*limit, 2*limit),
		"big.json": `{"description":"` + strings.Repeat("a", int(limit)) + `"}`,
	})

	out := filepath.Join(t.TempDir(), "out")

	res, err := Run(t.Context(), Options{Tool: pinnedTool(t), SourceFile: filepath.Join(dir, "source.toml"), OutDir: out})
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Regressions) != 1 || res.Regressions[0].Reason != ReasonTooLarge {
		t.Errorf("regressions = %+v", res.Regressions)
	}

	if entries := mustLoad(t, out).Document.Entries; len(entries) != 0 {
		t.Errorf("entries = %+v", entries)
	}
}
