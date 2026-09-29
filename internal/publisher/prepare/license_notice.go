package prepare

import (
	"strings"

	"github.com/ovineko/schepherd/internal/publisher/upstream"
)

// upstreamNotice is what the SchemaStore repository adds to the notice
// layer of a schema with documents from its snapshot: its LICENSE file and
// its NOTICE file, both of which Apache-2.0 (section 4) requires copies to
// carry. It does not name the commit, so it changes only when the texts do.
func upstreamNotice(snap *upstream.Snapshot) string {
	parts := make([]string, 0, 2)

	for _, text := range [][]byte{snap.License(), snap.Notice()} {
		if trimmed := strings.TrimRight(string(text), "\r\n"); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}

	return strings.Join(parts, "\n\n")
}
