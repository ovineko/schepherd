// Package calver implements catalog revisions.
//
// A revision is the UTC minute a catalog snapshot was published, written
// YYYYMMDD.HHMM with a real calendar date from 2000 to 9999 and a time from
// 00:00 to 23:59. It is not a counter: nothing about earlier revisions is
// needed to pick the next one, and the fixed width makes the textual order
// of revisions their chronological order. The OCI and Git tag of a revision
// is catalog-YYYYMMDD.HHMM.
package calver

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// TagPrefix is prepended to a revision to form its OCI and Git tag.
const TagPrefix = "catalog-"

const (
	layout  = "20060102.1504"
	minYear = 2000
	maxYear = 9999
	// maxQuoted bounds how much of a rejected input an error repeats.
	maxQuoted = 32
)

// ErrInvalid is returned for strings that are not valid revisions.
var ErrInvalid = errors.New("invalid catalog revision")

// Revision is a parsed catalog revision. The zero value is no revision.
type Revision struct {
	minute time.Time
}

// RevisionAt returns the revision of the UTC minute that contains t. The
// result is only valid for years 2000 to 9999; Check reports others.
func RevisionAt(t time.Time) Revision {
	return Revision{minute: t.UTC().Truncate(time.Minute)}
}

// ParseRevision accepts exactly YYYYMMDD.HHMM with a real UTC calendar date
// from 2000 to 9999 and a time from 00:00 to 23:59.
func ParseRevision(s string) (Revision, error) {
	if !wellFormed(s) {
		return Revision{}, fmt.Errorf("%w %s: expected YYYYMMDD.HHMM", ErrInvalid, quote(s))
	}

	minute, err := time.ParseInLocation(layout, s, time.UTC)
	if err != nil || minute.Format(layout) != s {
		return Revision{}, fmt.Errorf("%w %s: not a real UTC date and time", ErrInvalid, quote(s))
	}

	if year := minute.Year(); year < minYear || year > maxYear {
		return Revision{}, fmt.Errorf("%w %s: year %d is outside %d-%d", ErrInvalid, quote(s), year, minYear, maxYear)
	}

	return Revision{minute: minute}, nil
}

// ParseRevisionTag parses a tag catalog-YYYYMMDD.HHMM.
func ParseRevisionTag(tag string) (Revision, error) {
	rest, ok := strings.CutPrefix(tag, TagPrefix)
	if !ok {
		return Revision{}, fmt.Errorf("%w tag %s: expected %sYYYYMMDD.HHMM", ErrInvalid, quote(tag), TagPrefix)
	}

	return ParseRevision(rest)
}

func wellFormed(s string) bool {
	if len(s) != len(layout) {
		return false
	}

	for i := range len(s) {
		switch {
		case i == 8:
			if s[i] != '.' {
				return false
			}
		case s[i] < '0' || s[i] > '9':
			return false
		}
	}

	return true
}

// Check reports whether r is a revision ParseRevision would accept.
func (r Revision) Check() error {
	switch year := r.minute.Year(); {
	case r.IsZero():
		return fmt.Errorf("%w: no revision", ErrInvalid)
	case year < minYear || year > maxYear:
		return fmt.Errorf("%w: year %d is outside %d-%d", ErrInvalid, year, minYear, maxYear)
	}

	return nil
}

// IsZero reports whether r is the zero value.
func (r Revision) IsZero() bool {
	return r.minute.IsZero()
}

// String returns YYYYMMDD.HHMM, or "" for the zero value.
func (r Revision) String() string {
	if r.IsZero() {
		return ""
	}

	return r.minute.Format(layout)
}

// Tag returns catalog-YYYYMMDD.HHMM.
func (r Revision) Tag() string {
	return TagPrefix + r.String()
}

// Time returns the UTC minute of the revision.
func (r Revision) Time() time.Time {
	return r.minute
}

// Compare orders revisions chronologically, which is also the byte order of
// their strings.
func Compare(a, b Revision) int {
	return a.minute.Compare(b.minute)
}

func quote(s string) string {
	if len(s) <= maxQuoted {
		return strconv.Quote(s)
	}

	cut := maxQuoted
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return strconv.Quote(s[:cut]) + "..."
}
