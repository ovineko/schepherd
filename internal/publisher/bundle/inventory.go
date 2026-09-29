package bundle

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ovineko/schepherd/internal/fault"
)

// resolutionKeywords are the keywords whose values decide what a reference
// resolves to, dynamic scope included. The bundler embeds documents and
// never has a reason to add, drop or rewrite one of them.
var resolutionKeywords = map[string]struct{}{
	"$ref": {}, "$dynamicRef": {}, "$recursiveRef": {},
	"$anchor": {}, "$dynamicAnchor": {}, "$recursiveAnchor": {},
}

// keywordUse is one member named after a resolution keyword together with
// its scalar value in JSON encoding.
type keywordUse struct {
	keyword string
	value   string
}

// inventory counts the scalar members named after resolution keywords at
// any depth. The scan is syntactic, so a member that is not a keyword (a
// property name, an example) is counted too; it is counted the same way in
// the sources and in the bundle.
func inventory(data []byte, counts map[keywordUse]int) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	type frame struct {
		object  bool
		wantKey bool
	}

	var (
		stack   []frame
		pending string
	)

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			if len(stack) > 0 || pending != "" {
				return io.ErrUnexpectedEOF
			}

			return nil
		}

		if err != nil {
			return fmt.Errorf("scan members: %w", err)
		}

		top := len(stack) - 1

		if key, ok := tok.(string); ok && top >= 0 && stack[top].object && stack[top].wantKey {
			stack[top].wantKey = false

			pending = ""
			if _, ok := resolutionKeywords[key]; ok {
				pending = key
			}

			continue
		}

		if delim, ok := tok.(json.Delim); ok && (delim == '}' || delim == ']') {
			stack = stack[:top]
			pending = ""

			continue
		}

		if top >= 0 && stack[top].object {
			stack[top].wantKey = true
		}

		if delim, ok := tok.(json.Delim); ok {
			stack = append(stack, frame{object: delim == '{', wantKey: delim == '{'})
			pending = ""

			continue
		}

		if pending != "" {
			value, err := inventoryValue(tok)
			if err != nil {
				return fmt.Errorf("%s: %w", pending, err)
			}

			counts[keywordUse{keyword: pending, value: value}]++
			pending = ""
		}
	}
}

// inventoryValue encodes a scalar for comparison. The bundler rewrites
// number literals (2.50 becomes 2.5, -0 becomes 0), so a number is compared
// by its value; a number is never a reference or an anchor, only a member
// that happens to share a keyword's name.
func inventoryValue(tok json.Token) (string, error) {
	if number, ok := tok.(json.Number); ok {
		if f, err := strconv.ParseFloat(string(number), 64); err == nil {
			if f == 0 {
				f = 0
			}

			return strconv.FormatFloat(f, 'g', -1, 64), nil
		}
	}

	value, err := json.Marshal(tok)
	if err != nil {
		return "", fmt.Errorf("encode %v: %w", tok, err)
	}

	return string(value), nil
}

// checkInventory rejects a bundle that does not carry exactly the
// resolution keywords of the documents the CLI bundled. The behaviour
// comparison only sees what the test instances exercise, and a bundle
// without instances would otherwise pass on structural checks alone; this
// check notices a dropped, added or rewritten reference or anchor (such as
// a $dynamicRef turned into a $ref) whatever the instances.
func checkInventory(closure *Closure, schema []byte) error {
	want := map[keywordUse]int{}

	for _, n := range closure.nodes {
		if err := inventory(n.prepared, want); err != nil {
			return fault.Wrap(fault.Internal, err, "scan %s", n.doc.URI)
		}
	}

	got := map[keywordUse]int{}
	if err := inventory(schema, got); err != nil {
		return fault.Wrap(fault.Internal, err, "scan the bundle of %s", closure.Root.URI)
	}

	uses := slices.Collect(maps.Keys(want))
	for use := range got {
		if _, ok := want[use]; !ok {
			uses = append(uses, use)
		}
	}

	slices.SortFunc(uses, func(a, b keywordUse) int {
		return cmp.Or(strings.Compare(a.keyword, b.keyword), strings.Compare(a.value, b.value))
	})

	for _, use := range uses {
		if want[use] != got[use] {
			return reject(ReasonReferenceMismatch, "the bundle of %s has %d %s %s, its sources %d",
				closure.Root.URI, got[use], use.keyword, use.value, want[use])
		}
	}

	return nil
}
