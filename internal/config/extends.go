package config

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ovineko/schepherd/internal/fault"
)

const (
	maxExtendsDepth = 16
	// maxLayers bounds how many files an extends graph may expand to. A base
	// reached through several paths is applied once per path, so without a
	// bound a diamond-shaped graph grows exponentially with its depth.
	maxLayers    = 256
	maxFileBytes = 1 << 20
)

type loader struct {
	parsed map[string]*layer
	stack  []string
	order  []*layer
}

func newLoader() *loader {
	return &loader{parsed: map[string]*layer{}}
}

func (l *loader) visit(file string, depth int, via *extendsRef) error {
	if i := slices.Index(l.stack, file); i >= 0 {
		chain := append(slices.Clone(l.stack[i:]), file)

		return fault.New(fault.Usage, "%s: extends cycle: %s", via.at, strings.Join(chain, " -> "))
	}

	if depth > maxExtendsDepth {
		return fault.New(fault.Usage, "%s: extends chain is deeper than %d levels", via.at, maxExtendsDepth)
	}

	ly, err := l.parse(file, via)
	if err != nil {
		return err
	}

	l.stack = append(l.stack, file)

	for i := range ly.extends {
		ref := &ly.extends[i]

		if err := l.visit(absPath(filepath.Dir(file), ref.path), depth+1, ref); err != nil {
			return err
		}
	}

	l.stack = l.stack[:len(l.stack)-1]

	if len(l.order) == maxLayers {
		return fault.New(fault.Usage, "%s: extends expands to more than %d files", file, maxLayers)
	}

	l.order = append(l.order, ly)

	return nil
}

func (l *loader) parse(file string, via *extendsRef) (*layer, error) {
	if ly, ok := l.parsed[file]; ok {
		return ly, nil
	}

	context := "cannot read configuration file"
	if via != nil {
		context = via.at.String() + ": cannot read base configuration"
	}

	data, err := readConfigFile(file, context)
	if err != nil {
		return nil, err
	}

	ly, err := parseLayer(file, data)
	if err != nil {
		return nil, err
	}

	l.parsed[file] = ly

	return ly, nil
}

func (l *loader) files() []string {
	out := make([]string, 0, len(l.parsed))

	for _, ly := range l.order {
		if !slices.Contains(out, ly.file) {
			out = append(out, ly.file)
		}
	}

	return out
}

// readConfigFile refuses anything but a regular file before opening it, so a
// FIFO or device named by mistake cannot block or stream without end.
func readConfigFile(path, context string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "%s", context)
	}

	if !info.Mode().IsRegular() {
		return nil, fault.New(fault.Usage, "%s: %s is not a regular file", context, path)
	}

	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "%s", context)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "%s: read %s", context, path)
	}

	if len(data) > maxFileBytes {
		return nil, fault.New(fault.Usage, "%s: %s is larger than %d bytes", context, path, maxFileBytes)
	}

	return data, nil
}

func absPath(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}

	return filepath.Join(base, p)
}
