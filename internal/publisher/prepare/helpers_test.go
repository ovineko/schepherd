package prepare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/bundle"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

var (
	repoRoot     = filepath.Join("..", "..", "..")
	fixturesDir  = filepath.Join(repoRoot, "testdata", "publisher")
	localBasic   = filepath.Join(fixturesDir, "local-basic")
	schemaFile   = filepath.Join(repoRoot, "api", "prepared.schema.json")
	fixedNow     = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	toolOnce     sync.Once
	toolInstance *bundle.Tool
	errTool      error
)

// pinnedTool returns the pinned bundler installed in the module. Its absence
// fails the test: preparation cannot be verified without it.
func pinnedTool(t *testing.T) *bundle.Tool {
	t.Helper()

	toolOnce.Do(func() {
		name := "jsonschema"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}

		path, err := filepath.Abs(filepath.Join(repoRoot, ".tools", "bin", name))
		if err != nil {
			errTool = err

			return
		}

		toolInstance, errTool = bundle.FindTool(path, bundle.PinnedVersion)
	})

	if errTool != nil {
		t.Fatalf("the pinned JSON Schema CLI is unusable (install it with 'go run ./tools/install-jsonschema' from the module root): %v", errTool)
	}

	return toolInstance
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// readTree returns every regular file below dir by slash-separated
// relative name.
func readTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()

	files := map[string][]byte{}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		files[filepath.ToSlash(rel)] = data

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return files
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func readReport(t *testing.T, dir string) Report {
	t.Helper()

	var r Report
	readJSON(t, filepath.Join(dir, ReportFile), &r)

	return r
}

func recordByURL(t *testing.T, r Report, url string) RecordReport {
	t.Helper()

	for _, rec := range r.Records {
		if rec.URL == url {
			return rec
		}
	}

	t.Fatalf("report has no record for %s", url)

	return RecordReport{}
}

func entryByID(t *testing.T, set *Set, id string) Entry {
	t.Helper()

	for _, e := range set.Document.Entries {
		if e.ID == id {
			return e
		}
	}

	t.Fatalf("prepared set has no entry %q", id)

	return Entry{}
}

func wantKind(t *testing.T, err error, kind fault.Kind) {
	t.Helper()

	if err == nil {
		t.Fatalf("err = nil, want %s", kind)
	}

	if got := fault.KindOf(err); got != kind {
		t.Fatalf("err = %v (kind %s), want kind %s", err, got, kind)
	}
}

func mustLoad(t *testing.T, dir string) *Set {
	t.Helper()

	set, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(%s): %v", dir, err)
	}

	return set
}

func sha(s string) string {
	return digest.FromBytes([]byte(s))
}

// stateRecord is one schema of a test state.
type stateRecord struct {
	id, source string
	removed    bool
	excluded   bool
}

// stateOf returns a valid publisher state with the given records, as the
// last publication at 20260916.0300 left it.
func stateOf(records ...stateRecord) *state.State {
	const revision = "20260916.0300"

	s := &state.State{
		FormatVersion: state.FormatVersion,
		Source:        state.Source{Kind: state.KindLocal, Name: "previous"},
		Recipe:        Recipe,
		Catalog:       state.Catalog{Revision: revision, Digest: sha("catalog"), Size: 400},
	}

	for _, r := range records {
		rec := state.Schema{
			ID: r.id, ContentDigest: sha("content " + r.id), FirstRevision: "20260909.0300", LastChangedRevision: "20260909.0300",
			Entry: catalog.Entry{
				ID: r.id, Name: r.id, Artifact: catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: sha(r.id), Size: 1},
				Provenance: &catalog.Provenance{Source: r.source},
			},
		}

		if r.removed {
			rec.HeldSinceRevision, rec.HeldReason = revision, state.HeldRemovedUpstream
		}

		if r.excluded {
			rec.ExcludedRevision = revision
		}

		s.Schemas = append(s.Schemas, rec)
	}

	slices.SortFunc(s.Schemas, func(a, b state.Schema) int { return strings.Compare(a.ID, b.ID) })

	if err := s.Validate(); err != nil {
		panic(err)
	}

	return s
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()

	if !strings.Contains(haystack, needle) {
		t.Fatalf("%q does not contain %q", haystack, needle)
	}
}

func compactJSON(t *testing.T, s string) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// logRecorder collects the log lines of a run, which may log from several
// goroutines.
type logRecorder struct {
	lines []string
	mu    sync.Mutex
}

func (l *logRecorder) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logRecorder) find(t *testing.T, parts ...string) string {
	t.Helper()

	l.mu.Lock()
	defer l.mu.Unlock()

	for _, line := range l.lines {
		found := true

		for _, part := range parts {
			found = found && strings.Contains(line, part)
		}

		if found {
			return line
		}
	}

	t.Fatalf("no log line contains %q; logged:\n%s", parts, strings.Join(l.lines, "\n"))

	return ""
}
