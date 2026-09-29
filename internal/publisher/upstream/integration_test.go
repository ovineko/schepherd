package upstream

import (
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/ovineko/schepherd/internal/publisher/ids"
	"github.com/ovineko/schepherd/internal/publisher/policy"
)

const integrationPolicy = `
[[rules]]
id = "schemastore"
decision = "allow"
hosts = ["www.schemastore.org", "json.schemastore.org"]
license = "Apache-2.0"
notice = "Store notice"
reason = "store license"

[[rules]]
id = "schemastore-raw"
decision = "allow"
hosts = ["raw.githubusercontent.com"]
path_prefix = "/SchemaStore/schemastore/"
license = "Apache-2.0"
notice = "Store notice"
reason = "store license"
`

// TestSnapshotToDecisions runs the publisher's source pipeline on a synthetic
// snapshot: canonicalize catalog URLs, read the local files, assign IDs and
// decide licenses.
func TestSnapshotToDecisions(t *testing.T) {
	snap := fetchTestSnapshot(t)

	entries, err := snap.Catalog()
	if err != nil {
		t.Fatal(err)
	}

	policyPath := filepath.Join(t.TempDir(), "licenses.toml")
	if err := os.WriteFile(policyPath, []byte(integrationPolicy), 0o600); err != nil {
		t.Fatal(err)
	}

	pol, err := policy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}

	sources := make([]string, 0, len(entries))
	decisions := map[string]policy.Verdict{}

	for _, e := range entries {
		source := e.URL

		if canonical, ok := snap.CanonicalURL(e.URL); ok {
			source = canonical

			local, _ := snap.LocalPath(e.URL)
			if _, err := os.ReadFile(local); err != nil {
				t.Fatalf("local file for %s: %v", e.URL, err)
			}
		}

		sources = append(sources, source)
		decisions[source] = pol.Decide(source, nil).Decision
	}

	assigned, collisions, err := ids.Assign(sources, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	wantIDs := map[string]string{
		"https://www.schemastore.org/a.json":        "a",
		"https://www.schemastore.org/b.json":        "b",
		"https://www.schemastore.org/tsconfig.json": "tsconfig",
		"https://example.com/external.json":         "external",
	}

	if !maps.Equal(assigned, wantIDs) || len(collisions) != 0 {
		t.Fatalf("ids = %v, collisions = %v", assigned, collisions)
	}

	wantDecisions := map[string]policy.Verdict{
		"https://www.schemastore.org/a.json":        policy.Allow,
		"https://www.schemastore.org/b.json":        policy.Allow,
		"https://www.schemastore.org/tsconfig.json": policy.Allow,
		"https://example.com/external.json":         policy.Review,
	}

	if !maps.Equal(decisions, wantDecisions) {
		t.Fatalf("decisions = %v", decisions)
	}

	b := pol.Decide("https://www.schemastore.org/b.json", []string{"https://json.schemastore.org/a.json"})
	if b.Decision != policy.Allow || b.License != "Apache-2.0" || b.Notice != "Store notice" {
		t.Fatalf("b with dependency = %#v", b)
	}

	positive, negative := snap.Tests("a.json")
	if len(positive) != 2 || len(negative) != 1 {
		t.Fatalf("tests for a: %v %v", positive, negative)
	}
}
