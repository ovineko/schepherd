package notices

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/licensecheck"

	"github.com/ovineko/schepherd/tools/release/internal/licenses"
)

// acceptedLicenses are the SPDX identifiers, as licensecheck reports them,
// that a license file of the code may have. Any other license, however
// permissive, needs a review before it is added here.
var acceptedLicenses = []string{"Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "MIT"}

// goPatents is licensecheck's identifier of the Go project's PATENTS file.
const goPatents = "GooglePatentsFile"

// The rule by which a file consists of exactly one license. licensecheck
// splits a file into matches of known licenses and reports the share of
// its words they cover; it counts copyright lines up to 50 words before a
// match as part of the match. A file consists of one license when
//
//   - every match is of the same license and none is a bare URL (a license
//     may match twice, as Apache-2.0 does with its terms and the boilerplate
//     notice of its appendix),
//   - the matches cover at least minCoverage percent of the words,
//   - only whitespace stands between the matches and after the last one, and
//   - at most maxTitleWords words, a title such as "The MIT License (MIT)",
//     stand before the first match.
//
// An unknown text, several licenses in one file, and a license with a
// clause added outside the matched terms therefore fail. Words that the
// patterns of licensecheck allow inside a license, such as a name in the BSD
// endorsement clause or a line between the copyright lines and the terms,
// are accepted as licensecheck accepts them.
const (
	minCoverage   = 95.0
	maxTitleWords = 8
)

// identify returns the licensecheck identifier of a file that consists of
// exactly one license by the rule above.
func identify(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("the text is empty")
	}

	c := licensecheck.Scan([]byte(text))
	if len(c.Match) == 0 {
		return "", errors.New("licensecheck finds no license")
	}

	id := c.Match[0].ID

	var (
		names        []string
		mixed, split bool
	)

	for i, m := range c.Match {
		name := m.ID
		if m.IsURL {
			name += " (URL)"
		}

		names = append(names, name)
		mixed = mixed || m.IsURL || m.ID != id
		split = split || i > 0 && strings.TrimSpace(text[c.Match[i-1].End:m.Start]) != ""
	}

	found := strings.Join(names, " + ")

	switch {
	case mixed:
		return "", fmt.Errorf("licensecheck finds %s, not a single license", found)
	case split:
		return "", fmt.Errorf("licensecheck finds %s with other text between them", found)
	case strings.TrimSpace(text[c.Match[len(c.Match)-1].End:]) != "":
		return "", fmt.Errorf("licensecheck finds %s followed by other text", found)
	case len(strings.Fields(text[:c.Match[0].Start])) > maxTitleWords:
		return "", fmt.Errorf("licensecheck finds %s preceded by more than %d words", found, maxTitleWords)
	case c.Percent < minCoverage:
		return "", fmt.Errorf("licensecheck finds %s covering only %.1f%% of the text", found, c.Percent)
	}

	return id, nil
}

// classify returns the SPDX identifier of a license text, which must be one
// of acceptedLicenses by the rule of identify.
func classify(text string) (string, error) {
	id, err := identify(text)
	if err == nil && !slices.Contains(acceptedLicenses, id) {
		err = fmt.Errorf("licensecheck finds %s", id)
	}

	if err != nil {
		return "", fmt.Errorf("the license text is not recognized as exactly one of %s: %w; review the license and add it to acceptedLicenses",
			strings.Join(acceptedLicenses, ", "), err)
	}

	return id, nil
}

type fileRole int

const (
	roleUnknown fileRole = iota
	roleLicense
	roleDocumentation
	roleNotice
	rolePatents
)

// roleOf tells what a file that licenses.Collect reads is for: the license
// of the code (LICENSE, LICENCE or COPYING, optionally .txt or .md), the
// license of the documentation (LICENSE.docs), attribution notices (NOTICE)
// or a patent grant (PATENTS). Any other name, such as LICENSE-MIT next to
// LICENSE-APACHE, leaves open which code the file covers.
func roleOf(name string) fileRole {
	stem, ext, _ := strings.Cut(strings.ToUpper(name), ".")
	plain := ext == "" || ext == "TXT" || ext == "MD"

	switch {
	case (stem == "LICENSE" || stem == "LICENCE" || stem == "COPYING") && plain:
		return roleLicense
	case (stem == "LICENSE" || stem == "LICENCE") && ext == "DOCS":
		return roleDocumentation
	case stem == "NOTICE" && plain:
		return roleNotice
	case stem == "PATENTS" && plain:
		return rolePatents
	default:
		return roleUnknown
	}
}

// componentLicense classifies the license of a component or copied package
// from its license files: exactly one license file of the code, and a
// patent grant only when it is the Go project's.
func componentLicense(files []licenses.LicenseFile) (string, error) {
	var code []licenses.LicenseFile

	for _, f := range files {
		switch roleOf(f.Name) {
		case roleLicense:
			code = append(code, f)
		case roleDocumentation, roleNotice:
		case rolePatents:
			if id, err := identify(f.Text); err != nil || id != goPatents {
				return "", fmt.Errorf("%s is not a patent grant in the form of the Go project's PATENTS file", f.Name)
			}
		case roleUnknown:
			return "", fmt.Errorf("%s: cannot tell which code this license file covers", f.Name)
		}
	}

	if len(code) != 1 {
		return "", fmt.Errorf("found %d license files of the code, want exactly one LICENSE, LICENCE or COPYING", len(code))
	}

	id, err := classify(code[0].Text)
	if err != nil {
		return "", fmt.Errorf("%s: %w", code[0].Name, err)
	}

	return id, nil
}
