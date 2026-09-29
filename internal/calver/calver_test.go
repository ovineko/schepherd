package calver

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseRoundTrip(t *testing.T) {
	cases := map[string]time.Time{
		"20260924.0905": time.Date(2026, 9, 24, 9, 5, 0, 0, time.UTC),
		"20260924.1432": time.Date(2026, 9, 24, 14, 32, 0, 0, time.UTC),
		"20260101.0000": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		"20261231.2359": time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC),
		"20240229.1200": time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC),
		"20000101.0000": time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		"99991231.2359": time.Date(9999, 12, 31, 23, 59, 0, 0, time.UTC),
	}

	for input, want := range cases {
		r, err := ParseRevision(input)
		if err != nil {
			t.Fatalf("ParseRevision(%q): %v", input, err)
		}

		if got := r.String(); got != input {
			t.Errorf("String() = %q, want %q", got, input)
		}

		if !r.Time().Equal(want) || r.Time().Location() != time.UTC {
			t.Errorf("Time() of %q = %v, want %v", input, r.Time(), want)
		}

		if r.Tag() != "catalog-"+input {
			t.Errorf("Tag() = %q", r.Tag())
		}

		tag, err := ParseRevisionTag(r.Tag())
		if err != nil || Compare(tag, r) != 0 {
			t.Errorf("tag round trip of %q: %v", input, err)
		}

		if RevisionAt(want) != r {
			t.Errorf("RevisionAt(%v) = %v, want %v", want, RevisionAt(want), r)
		}
	}
}

func TestRevisionAtIsTheUTCMinute(t *testing.T) {
	berlin := time.FixedZone("UTC+2", 2*60*60)
	at := time.Date(2026, 9, 24, 11, 32, 59, 999999999, berlin)

	r := RevisionAt(at)
	if r.String() != "20260924.0932" {
		t.Errorf("RevisionAt(%v) = %s, want the UTC minute 20260924.0932", at, r)
	}

	if next := RevisionAt(at.Add(time.Nanosecond)); next.String() != "20260924.0933" || Compare(r, next) >= 0 {
		t.Errorf("the next nanosecond starts minute %s", next)
	}

	beforeMidnight := RevisionAt(time.Date(2026, 9, 24, 23, 59, 30, 0, time.UTC))
	afterMidnight := RevisionAt(time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC))

	if beforeMidnight.String() != "20260924.2359" || afterMidnight.String() != "20260925.0000" {
		t.Errorf("around midnight: %s, %s", beforeMidnight, afterMidnight)
	}

	if err := RevisionAt(time.Date(1999, 12, 31, 23, 59, 0, 0, time.UTC)).Check(); !errors.Is(err, ErrInvalid) {
		t.Errorf("a revision in 1999 passes Check: %v", err)
	}

	if err := RevisionAt(at).Check(); err != nil {
		t.Errorf("Check: %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	cases := []string{
		"",
		"20260924",
		"20260924.",
		"20260924.1",
		"20260924.12",
		"20260924.123",
		"20260924.12345",
		"20260924.2400",
		"20260924.1260",
		"20260924.9999",
		"20260230.1200",
		"20250229.1200",
		"20261301.1200",
		"20260001.1200",
		"20260900.1200",
		"20260431.1200",
		"19991231.2359",
		"00000101.0000",
		"2026924.1200",
		"020260924.1200",
		"20260924-1200",
		"20260924,1200",
		"2026-09-24.1200",
		"20260924.12:00",
		"20260924T1200",
		"20260924.1200 ",
		" 20260924.1200",
		"20260924.1200\n",
		"+20260924.1200",
		"20260924.+200",
		"２０２６0924.1200",
		"20260924.1200.1",
		"catalog-20260924.1200",
		"v0.20260924.1",
	}

	for _, input := range cases {
		r, err := ParseRevision(input)
		if err == nil || !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseRevision(%q) = %v, %v; want ErrInvalid", input, r, err)

			continue
		}

		if input != "" && len(input) < 20 && !strings.Contains(err.Error(), strconv.Quote(input)) {
			t.Errorf("the error for %q does not quote it: %v", input, err)
		}
	}
}

func TestParseRevisionTag(t *testing.T) {
	r, err := ParseRevisionTag("catalog-20260924.1432")
	if err != nil || r.String() != "20260924.1432" {
		t.Fatalf("ParseRevisionTag = %v, %v", r, err)
	}

	for _, tag := range []string{
		"20260924.1432", "catalog-", "catalog-20260924.1", "catalog-latest", "catalog-sha256-" + strings.Repeat("a", 64),
		"Catalog-20260924.1432", "catalog-20260924.2460", "schema-sha256-20260924.1432",
	} {
		if _, err := ParseRevisionTag(tag); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseRevisionTag(%q) = %v, want ErrInvalid", tag, err)
		}
	}
}

func TestErrorsQuoteBoundedInput(t *testing.T) {
	huge := strings.Repeat("9", 1<<20)

	for _, parse := range []func(string) (Revision, error){ParseRevision, ParseRevisionTag} {
		_, err := parse(huge)
		if err == nil || len(err.Error()) > 200 {
			t.Errorf("error for a 1 MiB input has %d bytes", len(err.Error()))
		}
	}

	_, err := ParseRevision(strings.Repeat("é", 40))
	if err == nil || !strings.Contains(err.Error(), "...") || strings.Contains(err.Error(), "\\x") {
		t.Errorf("a long multibyte input is not cut at a character boundary: %v", err)
	}
}

func TestCompareIsTimeAndStringOrder(t *testing.T) {
	inputs := []string{
		"20261231.2359", "20260924.1432", "20260924.0905", "20270101.0000", "20260924.0959", "20260925.0000", "20260924.1000",
	}

	revisions := make([]Revision, 0, len(inputs))

	for _, s := range inputs {
		r, err := ParseRevision(s)
		if err != nil {
			t.Fatal(err)
		}

		revisions = append(revisions, r)
	}

	slices.SortFunc(revisions, Compare)

	byTime := make([]string, 0, len(revisions))
	for _, r := range revisions {
		byTime = append(byTime, r.String())
	}

	byText := slices.Clone(inputs)
	slices.Sort(byText)

	if !slices.Equal(byTime, byText) {
		t.Errorf("time order %v differs from string order %v", byTime, byText)
	}

	want := []string{
		"20260924.0905", "20260924.0959", "20260924.1000", "20260924.1432", "20260925.0000", "20261231.2359", "20270101.0000",
	}
	if !slices.Equal(byTime, want) {
		t.Errorf("sorted = %v, want %v", byTime, want)
	}

	a, _ := ParseRevision("20260924.1432")
	if Compare(a, RevisionAt(a.Time().Add(59*time.Second))) != 0 {
		t.Error("two instants of the same minute compare unequal")
	}
}

func TestZeroValue(t *testing.T) {
	var zero Revision

	if !zero.IsZero() || zero.String() != "" || !errors.Is(zero.Check(), ErrInvalid) {
		t.Errorf("zero value: IsZero %v, String %q, Check %v", zero.IsZero(), zero.String(), zero.Check())
	}

	r, _ := ParseRevision("20260924.1432")
	if r.IsZero() || Compare(zero, r) >= 0 {
		t.Errorf("a parsed revision is zero or not newer than the zero value")
	}
}
