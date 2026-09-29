package notes

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// writeSections never writes more than its budget, whatever the budget and
// the lengths of the sections, and lists everything when it all fits.
func TestWriteSectionsStaysWithinTheBudget(t *testing.T) {
	lines := func(n, width int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("- %04d %s", i, strings.Repeat("x", width))
		}

		return out
	}

	sections := []section{
		{title: "Long", intro: "An intro.", lines: lines(300, 60)},
		{title: "Empty"},
		{title: "Short", lines: lines(2, 5)},
		{title: "Counted", lines: lines(3, 1), count: 40},
		{title: "Wide", lines: lines(20, 400)},
	}

	full := 0
	for i := range sections {
		full += sections[i].size("##")
	}

	for budget := -10; budget <= full+10; budget += 7 {
		var b bytes.Buffer

		writeSections(&b, budget, "##", sections)

		if b.Len() > max(budget, 0) {
			t.Fatalf("budget %d: wrote %d bytes", budget, b.Len())
		}

		if budget >= full && (b.Len() != full || strings.Contains(b.String(), "more not listed")) {
			t.Fatalf("budget %d of %d: wrote %d bytes:\n%s", budget, full, b.Len(), b.String())
		}
	}

	// Wide fits in its share of 20000 bytes and is listed in full; of
	// 10000 bytes it gets less than it needs, like Long.
	for budget, trimmed := range map[int]int{20000: 1, 10000: 2} {
		var b bytes.Buffer

		writeSections(&b, budget, "##", sections)

		for _, want := range []string{"## Counted (40)\n\n- 0000 x\n- 0001 x\n- 0002 x\n", "## Short (2)\n\n- 0000 xxxxx\n- 0001 xxxxx\n", "## Long (300)\n\nAn intro.\n\n- 0000 ", "## Wide (20)\n\n- 0000 "} {
			if !strings.Contains(b.String(), want) {
				t.Errorf("budget %d: sections lack %q", budget, want)
			}
		}

		if strings.Contains(b.String(), "Empty") || strings.Count(b.String(), "more not listed here\n") != trimmed {
			t.Errorf("budget %d: %d bytes with %d sections trimmed", budget, b.Len(), strings.Count(b.String(), "more not listed here\n"))
		}
	}
}
