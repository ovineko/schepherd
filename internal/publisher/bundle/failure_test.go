package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
)

// reasonOf returns the reason of the first Failure in err's chain, or "" if
// there is none.
func reasonOf(err error) string {
	if failure, ok := errors.AsType[*Failure](err); ok {
		return failure.Reason
	}

	return ""
}

func TestFailureErrorAndUnwrap(t *testing.T) {
	cause := errors.New("connection refused")
	failure := rejectWrap(ReasonUnresolvedRef, cause, "cannot fetch %s", "https://example.com/a.json")

	if got := failure.Error(); got != "bundle rejected (unresolved-ref): cannot fetch https://example.com/a.json: connection refused" {
		t.Errorf("Error() = %q", got)
	}

	wrapped := fmt.Errorf("prepare: %w", failure)

	if !errors.Is(wrapped, cause) {
		t.Error("the cause is not reachable through Unwrap")
	}

	if got := reasonOf(wrapped); got != ReasonUnresolvedRef {
		t.Errorf("reasonOf = %q", got)
	}

	if got := reasonOf(errors.New("plain")); got != "" {
		t.Errorf("reasonOf(plain) = %q", got)
	}

	if got := reject(ReasonFragmentRoot, "").Error(); got != "bundle rejected (fragment-root)" {
		t.Errorf("Error() without detail = %q", got)
	}
}

func TestClassifyCLIError(t *testing.T) {
	cases := map[string]string{
		"Could not resolve the reference to an external schema":                                            ReasonUnresolvedRef,
		"Could not resolve schema reference":                                                               ReasonUnresolvedRef,
		"Could not resolve the metaschema of the schema":                                                   ReasonUnsupportedDialect,
		"Relative meta-schema URIs are not valid according to the JSON Schema specification":               ReasonUnsupportedDialect,
		"Could not determine the base dialect of the schema":                                               ReasonUndeclaredDialect,
		"A schema with a top-level `$ref` in JSON Schema Draft 7 and older dialects ignores every sibling": ReasonTopLevelRefDraft7,
		"Conflicting schemas for the same identifier":                                                      ReasonIDMismatch,
		"Failed to parse the JSON document":                                                                ReasonBundlerError,
		"":                                                                                                 ReasonBundlerError,
	}

	for msg, want := range cases {
		if got := classifyCLIError(msg); got != want {
			t.Errorf("classifyCLIError(%q) = %q, want %q", msg, got, want)
		}
	}
}

func TestCLIFailureParsesJSONAndSanitizesPaths(t *testing.T) {
	isolateTemp(t)

	ws, err := newWorkspace()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(ws.close)

	file, err := ws.write("https://example.com/root.json", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	out, err := ws.output("bundle")
	if err != nil {
		t.Fatal(err)
	}

	report, err := json.Marshal(cliError{
		Error:      "Could not resolve the reference to an external schema",
		Identifier: "https://example.com/x.json",
		FilePath:   file,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := out.Write(report); err != nil {
		t.Fatal(err)
	}

	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	got := ws.cliFailure(runResult{stdout: out.Name(), exit: 4, stderr: "noise " + ws.dir})

	failure, ok := errors.AsType[*Failure](got)
	if !ok || failure.Reason != ReasonUnresolvedRef {
		t.Fatalf("cliFailure = %v", got)
	}

	if strings.Contains(failure.Detail, ws.dir) || !strings.Contains(failure.Detail, "in https://example.com/root.json") ||
		!strings.Contains(failure.Detail, `"https://example.com/x.json"`) {
		t.Errorf("detail = %q", failure.Detail)
	}

	if err := writeFile(out.Name(), "not json"); err != nil {
		t.Fatal(err)
	}

	got = ws.cliFailure(runResult{stdout: out.Name(), exit: 1, stderr: "crash reading " + fileURI(file)})
	if failure, ok := errors.AsType[*Failure](got); !ok || failure.Reason != ReasonBundlerError ||
		failure.Detail != "JSON Schema CLI exited with status 1: crash reading https://example.com/root.json" {
		t.Errorf("fallback failure = %v", got)
	}

	got = ws.cliFailure(runResult{stdout: out.Name(), exit: 5, stderr: "unknown option"})
	if _, isFailure := errors.AsType[*Failure](got); isFailure || fault.KindOf(got) != fault.Internal {
		t.Errorf("argument error = %v, want an internal fault", got)
	}
}
