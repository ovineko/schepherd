package prepare

import (
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
)

// TestProvenanceCarriesNoLicenseDetections checks that neither
// api/prepared.schema.json nor the parser accepts detection data in the
// provenance: it is copied into the catalog entry, and the commit a branch
// resolves to would change the entry, and so the catalog, every time the
// branch moves. The pins live in the entry's licenseDecision instead.
func TestProvenanceCarriesNoLicenseDetections(t *testing.T) {
	schema := compilePreparedSchema(t)

	set := sampleSet()

	data, err := Encode(&set.Document)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ParseDocument(data); err != nil {
		t.Fatalf("sample refused by the parser: %v", err)
	}

	if err := validateAgainstSchema(schema, data); err != nil {
		t.Fatalf("sample refused by the schema: %v", err)
	}

	detection := `"licenseSources":[{"url":"https://raw.githubusercontent.com/owner/repo/main/s.json",` +
		`"source":"github:owner/repo@` + strings.Repeat("a", 40) + `","license":"MIT","licenseFile":"LICENSE",` +
		`"licenseDigest":"` + sha("MIT") + `"}],`

	withDetection := strings.Replace(string(data), `"license": "`, detection+`"license": "`, 1)
	if withDetection == string(data) {
		t.Fatal("the sample has no provenance license to insert before")
	}

	if _, err := ParseDocument([]byte(withDetection)); fault.KindOf(err) != fault.Integrity || !strings.Contains(err.Error(), `unknown field "licenseSources"`) {
		t.Errorf("parser did not refuse provenance.licenseSources as unknown: %v", err)
	}

	if validateAgainstSchema(schema, []byte(withDetection)) == nil {
		t.Error("api/prepared.schema.json accepted provenance.licenseSources")
	}
}
