package licensedetect

import (
	"bytes"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/publisher/policy"
)

// maxTextBytes bounds a license or NOTICE file; a longer one is not carried
// in a notice layer.
const maxTextBytes = 64 << 10

// file is a license or NOTICE file as served.
type file struct {
	name string
	data []byte
}

// finish completes a finding whose license was detected: it records the
// NOTICE file and composes the text the schema's notice layer carries. The
// text names the source by the ref or version the URL names, not by the
// commit it resolved to, so it only changes when the license texts change
// and every schema of the source shares one notice blob.
func finish(f policy.Finding, subject string, license, notice *file) policy.Finding {
	licenseText, problem := plainText(license)
	if problem != "" {
		return refusal(f, policy.RefusedNoLicense, "the license file "+strconv.Quote(license.name)+" of "+f.Source+" "+problem)
	}

	parts := []string{
		"License of " + subject + " (SPDX: " + f.License + "), file " + license.name + ":",
		licenseText,
	}

	if notice != nil {
		noticeText, problem := plainText(notice)
		if problem != "" {
			return refusal(f, policy.RefusedNoLicense, "the NOTICE file "+strconv.Quote(notice.name)+" of "+f.Source+" "+problem)
		}

		f.NoticeFile, f.NoticeDigest = notice.name, digest.FromBytes(notice.data)
		parts = append(parts, "NOTICE of "+subject+", file "+notice.name+":", noticeText)
	}

	f.Notice = strings.Join(parts, "\n\n")

	return f
}

func refusal(f policy.Finding, reason, detail string) policy.Finding {
	return policy.Finding{
		Source: f.Source, License: f.License, LicenseFile: f.LicenseFile, LicenseDigest: f.LicenseDigest, Failure: reason, Detail: detail,
	}
}

// plainText returns a file as notice text: UTF-8 without a byte order mark,
// line feeds for line ends, no trailing blank lines. It refuses what cannot
// be shown faithfully as plain text: other encodings, control characters
// other than tab and form feed, and bidirectional controls, which can make
// displayed text differ from its bytes.
func plainText(f *file) (string, string) {
	switch {
	case len(f.data) > maxTextBytes:
		return "", "is larger than " + strconv.Itoa(maxTextBytes) + " bytes"
	case !utf8.Valid(f.data):
		return "", "is not UTF-8 text"
	}

	data := bytes.TrimPrefix(f.data, []byte("\uFEFF"))
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
	text := strings.TrimRight(string(data), " \t\n")

	if strings.TrimSpace(text) == "" {
		return "", "is empty"
	}

	if strings.ContainsFunc(text, func(r rune) bool {
		return (unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\f') || unicode.Is(unicode.Bidi_Control, r)
	}) {
		return "", "contains control characters"
	}

	return text, ""
}
