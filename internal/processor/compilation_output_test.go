package processor

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ovineko/schepherd/internal/cache"
)

func TestCompiledOutputReadFromPreservesPartialReads(t *testing.T) {
	var output compiledOutput
	if _, err := output.Write([]byte("prefix:")); err != nil {
		t.Fatal(err)
	}
	n, err := output.ReadFrom(iotest.OneByteReader(strings.NewReader("template")))
	if err != nil || n != 8 || string(output.data) != "prefix:template" || output.exceeded {
		t.Fatalf("read=%d, data=%q, exceeded=%t, err=%v", n, output.data, output.exceeded, err)
	}
	sourceErr := errors.New("compiler output unavailable")
	n, err = output.ReadFrom(io.MultiReader(strings.NewReader("tail"), iotest.ErrReader(sourceErr)))
	if !errors.Is(err, sourceErr) || n != 4 || string(output.data) != "prefix:templatetail" || output.exceeded {
		t.Fatalf("read=%d, data=%q, exceeded=%t, err=%v", n, output.data, output.exceeded, err)
	}
}

func TestCompiledOutputReadFromBoundary(t *testing.T) {
	storage := make([]byte, cache.MaxCompiledBytes)
	for _, tc := range []struct {
		name     string
		suffix   string
		want     string
		exceeded bool
	}{
		{name: "below limit", suffix: "x", want: "x"},
		{name: "exact limit", suffix: "xy", want: "xy"},
		{name: "over limit", suffix: "xyz", want: "xy", exceeded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := compiledOutput{data: storage[:cache.MaxCompiledBytes-2]}
			source := bytes.NewReader([]byte(tc.suffix))
			n, err := output.ReadFrom(source)
			if err != nil || n != int64(len(tc.suffix)) || output.exceeded != tc.exceeded {
				t.Fatalf("read=%d, exceeded=%t, err=%v", n, output.exceeded, err)
			}
			if len(output.data) != cache.MaxCompiledBytes-2+len(tc.want) || string(output.data[cache.MaxCompiledBytes-2:]) != tc.want {
				t.Fatalf("retained=%d, tail=%q", len(output.data), output.data[cache.MaxCompiledBytes-2:])
			}
			if cap(output.data) != cache.MaxCompiledBytes || source.Len() != 0 {
				t.Fatalf("capacity=%d, undrained=%d", cap(output.data), source.Len())
			}
		})
	}
}
