package notes

import (
	"regexp"
	"strings"
	"testing"
)

var backtickRunAtEdge = regexp.MustCompile("^`+|`+$")

func FuzzCodeSpan(f *testing.F) {
	for _, seed := range []string{"", "plain", "`", "``x``", "a ` b", " x ", "@user #1", "line\nbreak", " ", "<b>"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		span := codeSpan(s)

		fence := backtickRunAtEdge.FindString(span)
		if fence == "" || !strings.HasSuffix(span, fence) || len(span) < 2*len(fence)+1 {
			t.Fatalf("codeSpan(%q) = %q is not fenced", s, span)
		}

		inner := span[len(fence) : len(span)-len(fence)]
		if strings.ContainsAny(inner, "\r\n\u2028\u2029") {
			t.Fatalf("codeSpan(%q) = %q breaks the line", s, span)
		}

		for _, run := range backtickRun.FindAllString(inner, -1) {
			if len(run) >= len(fence) {
				t.Fatalf("codeSpan(%q) = %q: an inner run of %d backticks closes the fence early", s, span, len(run))
			}
		}

		if strings.HasPrefix(inner, "`") || strings.HasSuffix(inner, "`") {
			t.Fatalf("codeSpan(%q) = %q: a backtick touches the fence", s, span)
		}
	})
}
