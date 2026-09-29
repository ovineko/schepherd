package bundle

import (
	"reflect"
	"testing"
)

func TestInventory(t *testing.T) {
	counts := map[keywordUse]int{}

	input := `{"$ref":"a.json","properties":{"$ref":{"$ref":"#/x"},"$anchor":{"type":"string"}},
		"items":[{"$dynamicRef":"#node","$dynamicAnchor":"node"},{"$recursiveRef":"#","$recursiveAnchor":true}],
		"examples":[{"$ref":"a.json"}],"$anchor":"top","enum":["$ref"],"$id":"https://example.com/x.json","x":{"$ref":7}}`

	if err := inventory([]byte(input), counts); err != nil {
		t.Fatal(err)
	}

	want := map[keywordUse]int{
		{"$ref", `"a.json"`}: 2, {"$ref", `"#/x"`}: 1, {"$ref", `7`}: 1,
		{"$dynamicRef", `"#node"`}: 1, {"$dynamicAnchor", `"node"`}: 1,
		{"$recursiveRef", `"#"`}: 1, {"$recursiveAnchor", `true`}: 1,
		{"$anchor", `"top"`}: 1,
	}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("inventory = %v\nwant %v", counts, want)
	}

	if err := inventory([]byte(`{"$ref":`), map[keywordUse]int{}); err == nil {
		t.Error("truncated JSON was accepted")
	}
}

func TestCheckInventory(t *testing.T) {
	closure := &Closure{
		Root: Document{URI: "https://example.com/root.json"},
		nodes: []*node{
			{doc: Document{URI: "https://example.com/root.json"}, prepared: []byte(`{"$ref":"dep.json","$dynamicAnchor":"n"}`)},
			{doc: Document{URI: "https://example.com/dep.json"}, prepared: []byte(`{"items":{"$dynamicRef":"#n"}}`)},
		},
	}

	good := `{"$ref":"dep.json","$dynamicAnchor":"n","$defs":{"https://example.com/dep.json":{"items":{"$dynamicRef":"#n"}}}}`
	if err := checkInventory(closure, []byte(good)); err != nil {
		t.Errorf("faithful bundle: %v", err)
	}

	for name, bundle := range map[string]string{
		"rewritten": `{"$ref":"dep.json","$dynamicAnchor":"n","$defs":{"d":{"items":{"$ref":"#n"}}}}`,
		"dropped":   `{"$ref":"dep.json","$defs":{"d":{"items":{"$dynamicRef":"#n"}}}}`,
		"added":     `{"$ref":"dep.json","$dynamicAnchor":"n","$defs":{"d":{"items":{"$dynamicRef":"#n"},"x":{"$ref":"#"}}}}`,
	} {
		if err := checkInventory(closure, []byte(bundle)); reasonOf(err) != ReasonReferenceMismatch {
			t.Errorf("%s: %v, want reference-mismatch", name, err)
		}
	}
}

func TestInventoryComparesNumbersByValue(t *testing.T) {
	scan := func(data string) map[keywordUse]int {
		counts := map[keywordUse]int{}
		if err := inventory([]byte(data), counts); err != nil {
			t.Fatal(err)
		}

		return counts
	}

	source := scan(`{"examples":[{"$ref":2.50},{"$ref":-0},{"$anchor":1e2},{"$ref":12345678901234567890123}]}`)
	rewritten := scan(`{"examples":[{"$ref":2.5},{"$ref":0},{"$anchor":100},{"$ref":1.2345678901234567890123e+22}]}`)

	if !reflect.DeepEqual(source, rewritten) {
		t.Errorf("number literals the bundler normalizes count differently:\nsource    %v\nrewritten %v", source, rewritten)
	}

	if reflect.DeepEqual(scan(`{"$ref":1}`), scan(`{"$ref":2}`)) || reflect.DeepEqual(scan(`{"$ref":1}`), scan(`{"$ref":"1"}`)) {
		t.Error("different values count as the same")
	}
}

// TestBundlerNumberFormattingIsNoReferenceChange bundles a schema whose
// annotations hold objects with a numeric "$ref" member: the bundler
// rewrites those literals, which changes no reference.
func TestBundlerNumberFormattingIsNoReferenceChange(t *testing.T) {
	r := (&run{
		web:     &fakeWeb{docs: map[string]string{"https://example.com/num/dep.json": `{"$schema":"` + draft7 + `","type":"string"}`}},
		rootURI: "https://example.com/num/root.json",
		root: `{"$schema":"` + draft7 + `","$id":"https://example.com/num/root.json","properties":{"a":{"$ref":"dep.json"}},
			"examples":[{"$ref":2.50},{"$ref":-0},{"$ref":1e2}]}`,
		instances: instances("ok.json", `{"a":"x"}`, "bad.json", `{"a":1}`),
	}).do(t)

	if p := r.wantBundled(t); p.Checked != (Checked{Valid: 1, Invalid: 1, Agreed: 2}) {
		t.Errorf("checked = %+v", p.Checked)
	}
}
