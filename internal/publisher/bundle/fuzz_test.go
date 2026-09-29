package bundle

import (
	"bytes"
	"encoding/json"
	"testing"
	"unicode/utf8"

	"github.com/ovineko/schepherd/internal/jsonutil"
)

const fuzzMaxInput = 1 << 16

func FuzzSetStringMember(f *testing.F) {
	for _, seed := range []string{
		`{}`, ` { } `, `{"a":1}`, `{"$id":"x","n":12345678901234567890123}`,
		"{\n \"s\" : \"\\u00e9\\/\" , \"$id\" : 1 }", `{"a":{"$id":"y"},"b":[{"$id":"z"}]}`, `true`, `[]`,
	} {
		f.Add([]byte(seed), "$id", "https://example.com/a.json")
	}

	f.Fuzz(func(t *testing.T, data []byte, key, value string) {
		if len(data) > fuzzMaxInput || !utf8.ValidString(key) || !utf8.ValidString(value) || jsonutil.Check(data, 0) != nil {
			return
		}

		tl, err := scanTopLevel(data)
		if err != nil {
			t.Fatalf("scanTopLevel rejected strict JSON %q: %v", data, err)
		}

		out, err := setStringMember(data, tl, key, value)
		if !tl.isObject {
			if err == nil {
				t.Fatalf("setStringMember accepted a non-object")
			}

			return
		}

		if err != nil {
			t.Fatalf("setStringMember(%q) = %v", data, err)
		}

		if err := jsonutil.Check(out, 0); err != nil {
			t.Fatalf("output %q is not strict JSON: %v", out, err)
		}

		before := decodeMembers(t, data)
		after := decodeMembers(t, out)

		var got string
		if err := json.Unmarshal(after[key], &got); err != nil || got != value {
			t.Fatalf("member %q = %s, want %q", key, after[key], value)
		}

		delete(before, key)
		delete(after, key)

		if len(before) != len(after) {
			t.Fatalf("member count changed: %d -> %d", len(before), len(after))
		}

		for name, raw := range before {
			if !bytes.Equal(raw, after[name]) {
				t.Fatalf("member %q changed from %s to %s", name, raw, after[name])
			}
		}

		if _, existed := tl.members[key]; !existed {
			outTL, err := scanTopLevel(out)
			if err != nil {
				t.Fatalf("rescan: %v", err)
			}

			first := outTL.members[key].start
			for name, s := range outTL.members {
				if name != key && s.start < first {
					t.Fatalf("inserted member is not first")
				}
			}
		}
	})
}

func decodeMembers(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()

	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}

	return members
}

func FuzzHasNonFragmentRef(f *testing.F) {
	for _, seed := range []string{
		`{"$ref":"#/a"}`, `{"$ref":"x.json"}`, `{"a":[{"$dynamicRef":"#m"},{"$recursiveRef":"y"}]}`,
		`["$ref","x"]`, `{"$ref":{"$ref":"x"}}`, `true`, `"$ref"`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzMaxInput || jsonutil.Check(data, 64) != nil {
			return
		}

		got, err := hasNonFragmentRef(data)
		if err != nil {
			t.Fatalf("hasNonFragmentRef(%q) = %v", data, err)
		}

		if want := referenceNonFragment(t, data); got != want {
			t.Fatalf("hasNonFragmentRef(%q) = %v, reference says %v", data, got, want)
		}
	})
}

func FuzzParseInspection(f *testing.F) {
	for _, seed := range []string{
		`{"locations":{"static":{"a":{"type":"resource"}}},"references":[{"origin":"/$ref","base":"b"}]}`,
		`{"mode":"static","locations":{"dynamic":{}},"references":[]}`,
		`{}`, `[]`, `{"locations":{"static":{"a":1}}}`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzMaxInput {
			return
		}

		in, err := parseInspection(bytes.NewReader(data))
		if err != nil {
			return
		}

		for _, ref := range in.external() {
			if _, internal := in.resources[ref.Base]; internal {
				t.Fatalf("internal reference %+v reported as external", ref)
			}
		}
	})
}

func FuzzCanonicalURI(f *testing.F) {
	for _, seed := range []string{
		"https://Example.COM:443/a/./b/../c.json?x=%7e#f", "http://ex%41mple.com/%e2%82%ac", "urn:x:%7E",
		"https://[::1]:80/..", "https://u@h/a/b/../../../", "%zz", "a/../b", "",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > fuzzMaxInput {
			return
		}

		once := canonicalURI(raw)
		if twice := canonicalURI(once); twice != once {
			t.Fatalf("canonicalURI is not idempotent on %q: %q, then %q", raw, once, twice)
		}
	})
}
