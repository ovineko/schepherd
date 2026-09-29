package jsonutil

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckRejectsDuplicateKeys(t *testing.T) {
	cases := []string{
		`{"a":1,"a":2}`,
		`{"a":1,"a":2}`,
		`{"x":{"b":[1,{"c":1,"c":1}]}}`,
		`[{"k":1},{"k":1,"k":2}]`,
	}

	for _, input := range cases {
		if err := Check([]byte(input), 0); !errors.Is(err, ErrDuplicateKey) {
			t.Errorf("Check(%s) = %v, want ErrDuplicateKey", input, err)
		}
	}
}

func TestCheckAcceptsSameKeyInSiblingObjects(t *testing.T) {
	cases := []string{
		`{"a":{"a":{"a":1}},"b":[{"a":1},{"a":2}]}`,
		`true`,
		`false`,
		`{}`,
		`[]`,
		`{"a":[],"b":{},"c":null}`,
	}

	for _, input := range cases {
		if err := Check([]byte(input), 0); err != nil {
			t.Errorf("Check(%s) = %v", input, err)
		}
	}
}

func TestCheckRejectsMalformed(t *testing.T) {
	cases := []string{"", "{", `{"a":1}{"b":2}`, `{"a":1,}`, "\xff\xfe", `{"a":01}`, `{"a":1} x`}

	for _, input := range cases {
		if err := Check([]byte(input), 0); err == nil {
			t.Errorf("Check(%q) accepted malformed input", input)
		}
	}
}

func TestCheckDepthLimit(t *testing.T) {
	deep := strings.Repeat("[", 10) + strings.Repeat("]", 10)

	if err := Check([]byte(deep), 10); err != nil {
		t.Errorf("depth 10 with limit 10: %v", err)
	}

	if err := Check([]byte(deep), 9); !errors.Is(err, ErrTooDeep) {
		t.Errorf("depth 10 with limit 9: %v, want ErrTooDeep", err)
	}
}

func TestCompactIsLossless(t *testing.T) {
	input := "{\n  \"big\": 12345678901234567890123,\n  \"dec\": 0.1000000000000000055511151231257827,\n  \"exp\": 1E400,\n  \"esc\": \"\\u00e9 \\\"q\\\" <&>\",\n  \"utf\": \"é\",\n  \"$comment\": \"keep\",\n  \"b\": true\n}\n"
	want := `{"big":12345678901234567890123,"dec":0.1000000000000000055511151231257827,"exp":1E400,"esc":"\u00e9 \"q\" <&>","utf":"é","$comment":"keep","b":true}`

	got, err := Compact([]byte(input), 0)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != want {
		t.Errorf("Compact() =\n%s\nwant\n%s", got, want)
	}
}

func TestCompactBooleanSchema(t *testing.T) {
	got, err := Compact([]byte(" true \n"), 0)
	if err != nil || string(got) != "true" {
		t.Errorf("Compact(true) = %q, %v", got, err)
	}
}

func FuzzCheck(f *testing.F) {
	for _, seed := range []string{`{"a":1}`, `{"a":1,"a":2}`, `[[[]]]`, `true`, `{"a":1,"a":1}`} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if err := Check(data, 64); err != nil {
			return
		}

		compacted, err := Compact(data, 64)
		if err != nil {
			t.Fatalf("Check accepted but Compact failed: %v", err)
		}

		if err := Check(compacted, 64); err != nil {
			t.Fatalf("compacted output no longer valid: %v", err)
		}
	})
}
