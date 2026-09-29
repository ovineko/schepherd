// Package notices generates THIRD_PARTY_NOTICES.md and checks the committed
// file against it. The file covers only what Schepherd distributes: the Go
// modules linked into the client for every release target, with version and
// license, and the sources of the schemas in the catalog that
// catalog/state.json records, grouped by the license decisions recorded
// there. Maintainer and test tooling is never distributed and never listed.
//
// Module licenses are identified from their license files with
// github.com/google/licensecheck. A file must consist of exactly one of a few
// accepted licenses (see identify); anything else stops the generator
// instead of being guessed.
package notices

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/tools/release/internal/licenses"
)

// Repository paths the notices are generated from and written to.
const (
	File      = "THIRD_PARTY_NOTICES.md"
	StateFile = "catalog/state.json"
)

// Options configures Generate and Check.
type Options struct {
	// Root is the repository root.
	Root string
	// State is the catalog state; empty means StateFile under Root.
	State string
	// Targets are the builds to cover; empty means the release targets.
	Targets []licenses.Target
	// Env is the environment of the go command.
	Env []string
}

func (o Options) withDefaults() Options {
	if o.Root == "" {
		o.Root = "."
	}

	if o.State == "" {
		o.State = filepath.Join(o.Root, filepath.FromSlash(StateFile))
	}

	if len(o.Targets) == 0 {
		o.Targets = licenses.ReleaseTargets()
	}

	return o
}

// Generate renders the notices from the repository at opts.Root and the
// state at opts.State.
func Generate(ctx context.Context, opts Options) ([]byte, error) {
	opts = opts.withDefaults()

	in := &inputs{targets: opts.Targets}

	var err error

	if in.data, err = loadData(opts.Root); err != nil {
		return nil, err
	}

	if in.rules, err = loadRules(opts.Root); err != nil {
		return nil, err
	}

	if in.state, err = state.Load(opts.State); err != nil {
		return nil, err //nolint:wrapcheck // classified by package state
	}

	in.client, err = licenses.Collect(ctx, licenses.Options{Root: opts.Root, Targets: opts.Targets, Env: opts.Env})
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "collect %s", licenses.ClientPackage)
	}

	return render(in)
}

// Check compares the committed notices with the generated ones and returns
// the differences; an error means the notices could not be generated.
func Check(ctx context.Context, opts Options) ([]string, error) {
	opts = opts.withDefaults()

	want, err := Generate(ctx, opts)
	if err != nil {
		return nil, err
	}

	const regenerate = "; run `go run ./tools/release notices generate`"

	have, err := os.ReadFile(filepath.Join(opts.Root, File))

	switch {
	case errors.Is(err, os.ErrNotExist):
		return []string{File + " is missing" + regenerate}, nil
	case err != nil:
		return nil, fault.Wrap(fault.Internal, err, "read %s", File)
	}

	have = bytes.ReplaceAll(have, []byte("\r\n"), []byte("\n"))
	if bytes.Equal(have, want) {
		return nil, nil
	}

	return []string{fmt.Sprintf("%s is out of date in %s%s", File, strings.Join(changedSections(have, want), ", "), regenerate)}, nil
}

// changedSections names the "## " sections whose text differs, or the
// introduction before them.
func changedSections(have, want []byte) []string {
	split := func(data []byte) map[string]string {
		sections := map[string]string{}
		name := "the introduction"

		for i, part := range strings.Split(string(data), "\n## ") {
			if i > 0 {
				heading, _, _ := strings.Cut(part, "\n")
				name = "section " + strings.TrimSpace(heading)
			}

			sections[name] += part
		}

		return sections
	}

	haveSections, wantSections := split(have), split(want)

	var changed []string

	for _, sections := range []map[string]string{wantSections, haveSections} {
		for name := range sections {
			if haveSections[name] != wantSections[name] && !slices.Contains(changed, name) {
				changed = append(changed, name)
			}
		}
	}

	if len(changed) == 0 {
		return []string{"its formatting"}
	}

	slices.Sort(changed)

	return changed
}
