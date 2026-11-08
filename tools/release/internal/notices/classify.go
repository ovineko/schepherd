package notices

import (
	"crypto/sha256"
	"encoding/hex"
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
var acceptedLicenses = []string{"Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "MIT", "MIT AND Apache-2.0"}

// goYAMLCombinedLicenseHash pins the normalized reviewed license text in go.yaml.in/yaml/v3 v3.0.5.
const goYAMLCombinedLicenseHash = "324926617f47a0cabd01eb385ae3bb7f7007b1ff0d3314461521f78c91dcac4a"

// goPatents is licensecheck's identifier of the Go project's PATENTS file.
const goPatents = "GooglePatentsFile"

// The rule by which a file consists of one license, or the specifically
// accepted MIT and Apache-2.0 combination. licensecheck splits a file into
// matches of known licenses and reports the share of its words they cover;
// it counts copyright lines up to 50 words before a match as part of the
// match. A single license file passes when every match is of the same
// license and none is a bare URL, the matches cover at least minCoverage
// percent of the words, only whitespace stands between matches and after
// the last one, and at most maxTitleWords words stand before the first.
// The combined form passes only when its exact declaration precedes one MIT
// and one Apache-2.0 text, the combined coverage meets minCoverage, and the
// file contains no trailing text. Unknown text, added clauses, and other
// combinations still fail.
const (
	minCoverage   = 95.0
	maxTitleWords = 8
)

// identify returns the licensecheck identifier of a file that consists of
// exactly one license by the rule above.
func identify(text string) (string, error) {
	data := []byte(strings.Trim(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\n") + "\n")
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) == goYAMLCombinedLicenseHash {
		return "MIT AND Apache-2.0", nil
	}

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
