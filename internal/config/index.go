package config

import (
	"slices"

	"github.com/pelletier/go-toml/v2/unstable"
)

// node is a key path in a document together with the byte offset of the key
// that introduced it, or -1 when the position is unknown.
type node struct {
	path string
	off  int
}

// indexer maps every key path of a document to the offset of its first
// occurrence. The generic map decode that validates the document carries no
// positions, so this second pass over the syntax tree supplies them for error
// messages. It assumes the document already decoded successfully.
type indexer struct {
	parser *unstable.Parser
	index  map[string]int
	arrays map[string]int
}

func indexKeys(data []byte) map[string]int {
	ix := &indexer{parser: &unstable.Parser{}, index: map[string]int{}, arrays: map[string]int{}}
	ix.parser.Reset(data)

	prefix := ""

	for ix.parser.NextExpression() {
		expr := ix.parser.Expression()

		if expr.Kind == unstable.KeyValue {
			ix.keyValue(prefix, expr)

			continue
		}

		if expr.Kind == unstable.Table || expr.Kind == unstable.ArrayTable {
			prefix = ix.header(expr.Key(), expr.Kind == unstable.ArrayTable)
		}
	}

	return ix.index
}

func (ix *indexer) offset(n *unstable.Node) int {
	return int(n.Raw.Offset)
}

func (ix *indexer) record(path string, off int) {
	if _, ok := ix.index[path]; !ok {
		ix.index[path] = off
	}
}

// header resolves a table header to the path the decoder sees: a segment
// naming an array of tables refers to its most recent element, and the last
// segment of an array-table header opens a new element.
func (ix *indexer) header(it unstable.Iterator, array bool) string {
	path := ""

	for it.Next() {
		key := it.Node()
		last := it.IsLast()
		off := ix.offset(key)
		path = joinKey(path, string(key.Data))
		ix.record(path, off)

		switch n := ix.arrays[path]; {
		case last && array:
			ix.arrays[path] = n + 1
			path = joinIndex(path, n)
			ix.index[path] = off
		case n > 0:
			path = joinIndex(path, n-1)
		}
	}

	return path
}

func (ix *indexer) keyValue(prefix string, kv *unstable.Node) {
	path := prefix
	off := -1

	it := kv.Key()
	for it.Next() {
		key := it.Node()
		off = ix.offset(key)
		path = joinKey(path, string(key.Data))
		ix.record(path, off)
	}

	ix.value(path, off, kv.Value())
}

func (ix *indexer) value(path string, off int, v *unstable.Node) {
	it := v.Children()

	if v.Kind == unstable.InlineTable {
		for it.Next() {
			if child := it.Node(); child.Kind == unstable.KeyValue {
				ix.keyValue(path, child)
			}
		}
	}

	if v.Kind == unstable.Array {
		for i := 0; it.Next(); {
			if elem := it.Node(); elem.Kind != unstable.Comment {
				p := joinIndex(path, i)
				ix.record(p, off)
				ix.value(p, off, elem)
				i++
			}
		}
	}
}

type lineIndex []int

func newLineIndex(data []byte) lineIndex {
	starts := lineIndex{0}

	for i, b := range data {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}

	return starts
}

func (li lineIndex) position(off int) (line, col int) {
	i, found := slices.BinarySearch(li, off)
	if !found {
		i--
	}

	return i + 1, off - li[i] + 1
}
