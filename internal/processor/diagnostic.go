package processor

import (
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// PathSubstitution names one local path and the stable token used in diagnostics.
type PathSubstitution struct {
	Path  string
	Token string
}

// NormalizeDiagnosticPaths replaces only caller-declared local path references.
// Remote URLs and surrounding validator wording remain unchanged.
func NormalizeDiagnosticPaths(message string, substitutions ...PathSubstitution) string {
	ordered := slices.Clone(substitutions)
	slices.SortFunc(ordered, func(a, b PathSubstitution) int {
		if len(a.Path) != len(b.Path) {
			return len(b.Path) - len(a.Path)
		}
		return strings.Compare(a.Path, b.Path)
	})
	for _, substitution := range ordered {
		if substitution.Path == "" || substitution.Token == "" {
			continue
		}
		path := filepath.Clean(substitution.Path)
		variants := []string{path, filepath.ToSlash(path), filepath.FromSlash(path)}
		if escaped := (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath(); escaped != path {
			variants = append(variants, escaped)
		}
		for _, variant := range slices.Compact(variants) {
			message = replaceLocalPath(message, variant, substitution.Token)
		}
	}
	return message
}

func replaceLocalPath(message, path, token string) string {
	var out strings.Builder
	out.Grow(len(message))
	for offset := 0; offset < len(message); {
		relative := strings.Index(message[offset:], path)
		if relative < 0 {
			out.WriteString(message[offset:])
			break
		}
		start := offset + relative
		end := start + len(path)
		if diagnosticPathBoundary(message, start, end) && !insideRemoteURL(message, start) {
			out.WriteString(message[offset:start])
			out.WriteString(token)
			offset = end
			continue
		}
		out.WriteString(message[offset : start+1])
		offset = start + 1
	}
	return out.String()
}

func diagnosticPathBoundary(message string, start, end int) bool {
	if start > 0 {
		previous := rune(message[start-1])
		if unicode.IsLetter(previous) || unicode.IsDigit(previous) || strings.ContainsRune("._-", previous) {
			return false
		}
	}
	if end < len(message) {
		next := rune(message[end])
		if !unicode.IsSpace(next) && !strings.ContainsRune("/\\#?:,;)]}'\"", next) {
			return false
		}
	}
	return true
}

func insideRemoteURL(message string, start int) bool {
	boundary := strings.LastIndexAny(message[:start], " \t\r\n\"'(<[{") + 1
	prefix := message[boundary:start]
	return strings.Contains(prefix, "://") && !strings.HasPrefix(prefix, "file://")
}
