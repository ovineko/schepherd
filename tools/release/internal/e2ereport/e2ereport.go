// Package e2ereport turns `go test -json` output into a Markdown report of the
// end-to-end scenarios. Scenario tests are named Test<ID>_<Name>, where ID is
// E followed by two or more digits (E01, E02, ...); their subtests are listed
// under the same ID. Every required scenario (RequiredScenarios) appears in
// the report, also when no test of it ran.
package e2ereport

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Results as shown in the report. A skipped test is reported as NOT RUN.
const (
	ResultPass   = "PASS"
	ResultFail   = "FAIL"
	ResultNotRun = "NOT RUN"
)

const maxLine = 16 << 20

var scenarioName = regexp.MustCompile(`^Test(E\d{2,})_[A-Za-z0-9_]+(?:/.*)?$`)

// Scenario is one required end-to-end scenario of the scenario matrix
// (docs/testing.md).
type Scenario struct {
	ID    string
	Title string
}

// RequiredScenarios is the scenario matrix every full end-to-end run must
// cover. A scenario without any test in the input is reported as NOT RUN, so
// a deleted, renamed or filtered scenario cannot disappear from the report.
var RequiredScenarios = []Scenario{
	{"E01", "Round trip"},
	{"E02", "Catalog only"},
	{"E03", "Only the requested schema"},
	{"E04", "Cache hit"},
	{"E05", "cat and export"},
	{"E06", "Compression"},
	{"E07", "One schema changed"},
	{"E08", "No-op publication"},
	{"E09", "Mutable tag"},
	{"E10", "Time-dependent metadata"},
	{"E11", "Full mirror"},
	{"E12", "Source independence"},
	{"E13", "Repeated mirror"},
	{"E14", "Partial mirror failure"},
	{"E15", "Retention"},
	{"E16", "Strict offline, full cache"},
	{"E17", "Offline miss"},
	{"E18", "Cache corruption"},
	{"E19", "Corrupt transport"},
	{"E20", "Resource limits"},
	{"E21", "Concurrency"},
	{"E22", "Path safety"},
	{"E23", "Batch grouping"},
	{"E24", "Per-file"},
	{"E25", "Stdin"},
	{"E26", "Environment and working directory"},
	{"E27", "No shell"},
	{"E28", "Interpolation"},
	{"E29", "Extends"},
	{"E30", "Exit status and cancellation"},
	{"E31", "Real validation"},
	{"E32", "Backend swap"},
	{"E33", "Matching"},
	{"E34", "Auth and TLS"},
	{"E35", "Catalog revisions"},
	{"E37", "npm packaging"},
	{"E38", "Self-contained bundle"},
	{"E39", "Dependency-only change"},
	{"E40", "Local schemas"},
	{"E41", "PyPI and RubyGems wrappers"},
	{"E42", "Generic OCI copy"},
}

// ScenarioResult is the outcome of one scenario: FAIL when any of its tests
// failed or did not finish, PASS when every one of its top-level tests
// passed, NOT RUN otherwise (no test in the input, or skipped).
type ScenarioResult struct {
	Scenario

	Result string
	// Tests counts the tests and subtests of the scenario in the input.
	Tests int
	// Required is false for a scenario ID that is not in RequiredScenarios.
	Required bool
}

// Row is one test or subtest of a scenario.
type Row struct {
	ID       string
	Test     string
	Package  string
	Result   string
	Elapsed  float64
	Finished bool
	HasTime  bool
}

// Report is the parsed test run. SkippedLines counts lines that looked like
// JSON but could not be decoded, such as the last line of a run that was
// cancelled while writing it.
type Report struct {
	Rows            []Row
	PackageFailures []string
	SkippedLines    int
}

type event struct {
	Elapsed    *float64 `json:"Elapsed"`
	Action     string   `json:"Action"`
	Package    string   `json:"Package"`
	ImportPath string   `json:"ImportPath"`
	Test       string   `json:"Test"`
}

// Parse reads a `go test -json` stream. Lines that are not JSON objects, such
// as stray tool output mixed into the file, are ignored; JSON lines that
// cannot be decoded are skipped and counted, so a truncated stream still
// yields a report of everything before the damage.
func Parse(r io.Reader) (*Report, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)

	rows := map[string]*Row{}

	var (
		order    []string
		failures []string
		skipped  int
	)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}

		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			skipped++

			continue
		}

		if ev.Test == "" {
			if failure := packageFailure(ev); failure != "" && !slices.Contains(failures, failure) {
				failures = append(failures, failure)
			}

			continue
		}

		m := scenarioName.FindStringSubmatch(ev.Test)
		if m == nil {
			continue
		}

		key := ev.Package + "\x00" + ev.Test

		row, ok := rows[key]
		if !ok {
			row = &Row{ID: m[1], Test: ev.Test, Package: ev.Package, Result: ResultFail}
			rows[key] = row
			order = append(order, key)
		}

		applyAction(row, ev)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read test output: %w", err)
	}

	report := &Report{PackageFailures: failures, SkippedLines: skipped}

	for _, key := range order {
		report.Rows = append(report.Rows, *rows[key])
	}

	slices.SortStableFunc(report.Rows, compareRows)

	return report, nil
}

func applyAction(row *Row, ev event) {
	switch ev.Action {
	case "pass":
		row.Result = ResultPass
	case "fail":
		row.Result = ResultFail
	case "skip":
		row.Result = ResultNotRun
	default:
		return
	}

	row.Finished = true

	if ev.Elapsed != nil {
		row.Elapsed = *ev.Elapsed
		row.HasTime = true
	}
}

func packageFailure(ev event) string {
	switch ev.Action {
	case "fail":
		return ev.Package
	case "build-fail":
		return ev.ImportPath + " (build failed)"
	default:
		return ""
	}
}

func compareRows(a, b Row) int {
	if c := idNumber(a.ID) - idNumber(b.ID); c != 0 {
		return c
	}

	return strings.Compare(a.Test, b.Test)
}

func idNumber(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "E"))
	if err != nil {
		return 0
	}

	return n
}

// Counts returns the number of rows per result.
func (r *Report) Counts() (pass, fail, notRun int) {
	for _, row := range r.Rows {
		switch row.Result {
		case ResultPass:
			pass++
		case ResultNotRun:
			notRun++
		default:
			fail++
		}
	}

	return pass, fail, notRun
}

// Scenarios returns one result per required scenario, in matrix order,
// followed by the scenario IDs of the input that are not required.
func (r *Report) Scenarios() []ScenarioResult {
	byID := map[string][]Row{}

	for _, row := range r.Rows {
		byID[row.ID] = append(byID[row.ID], row)
	}

	out := make([]ScenarioResult, 0, len(RequiredScenarios))
	required := map[string]bool{}

	for _, s := range RequiredScenarios {
		required[s.ID] = true
		out = append(out, scenarioResult(s, true, byID[s.ID]))
	}

	var extra []string

	for id := range byID {
		if !required[id] {
			extra = append(extra, id)
		}
	}

	slices.SortFunc(extra, func(a, b string) int { return idNumber(a) - idNumber(b) })

	for _, id := range extra {
		out = append(out, scenarioResult(Scenario{ID: id}, false, byID[id]))
	}

	return out
}

func scenarioResult(s Scenario, required bool, rows []Row) ScenarioResult {
	res := ScenarioResult{Scenario: s, Result: ResultNotRun, Tests: len(rows), Required: required}

	var top []Row

	for _, row := range rows {
		if row.Result == ResultFail || !row.Finished {
			res.Result = ResultFail

			return res
		}

		if !strings.Contains(row.Test, "/") {
			top = append(top, row)
		}
	}

	if len(top) == 0 {
		top = rows
	}

	if len(top) > 0 && !slices.ContainsFunc(top, func(row Row) bool { return row.Result != ResultPass }) {
		res.Result = ResultPass
	}

	return res
}

// ScenarioCounts returns the number of required scenarios per result.
func (r *Report) ScenarioCounts() (pass, fail, notRun int) {
	for _, s := range r.Scenarios() {
		if !s.Required {
			continue
		}

		switch s.Result {
		case ResultPass:
			pass++
		case ResultFail:
			fail++
		default:
			notRun++
		}
	}

	return pass, fail, notRun
}

// Unpassed lists the required scenarios that did not pass, as "<ID> (<result>)".
func (r *Report) Unpassed() []string {
	var out []string

	for _, s := range r.Scenarios() {
		if s.Required && s.Result != ResultPass {
			out = append(out, s.ID+" ("+s.Result+")")
		}
	}

	return out
}

// Markdown renders the report.
func (r *Report) Markdown() string {
	var b strings.Builder

	b.WriteString("# End-to-end test report\n\n## Scenarios\n\n| ID | Scenario | Result | Tests |\n| --- | --- | --- | --- |\n")

	for _, s := range r.Scenarios() {
		title := s.Title
		if !s.Required {
			title = "(not a required scenario)"
		}

		fmt.Fprintf(&b, "| %s | %s | %s | %d |\n", s.ID, title, s.Result, s.Tests)
	}

	pass, fail, notRun := r.ScenarioCounts()
	fmt.Fprintf(&b, "\n**Scenarios:** %d required: %d passed, %d failed, %d not run.\n\n## Tests\n\n",
		len(RequiredScenarios), pass, fail, notRun)

	if len(r.Rows) == 0 {
		b.WriteString("No scenario tests (Test<ID>_<Name>) were found in the input.\n")
	} else {
		b.WriteString("| ID | Test | Result | Duration |\n| --- | --- | --- | --- |\n")

		for _, row := range r.Rows {
			result := row.Result
			if !row.Finished {
				result += " (did not finish)"
			}

			duration := "-"
			if row.HasTime {
				duration = strconv.FormatFloat(row.Elapsed, 'f', 2, 64) + "s"
			}

			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", row.ID, strings.ReplaceAll(row.Test, "|", `\|`), result, duration)
		}
	}

	pass, fail, notRun = r.Counts()
	fmt.Fprintf(&b, "\n**Totals:** %d tests: %d passed, %d failed, %d not run.\n", len(r.Rows), pass, fail, notRun)

	if len(r.PackageFailures) > 0 {
		b.WriteString("\n**Package failures:** " + strings.Join(r.PackageFailures, ", ") + "\n")
	}

	if r.SkippedLines > 0 {
		fmt.Fprintf(&b, "\n**Warning:** %d line(s) of the input could not be decoded and were skipped; the run may have been cancelled or its output truncated, so results can be missing.\n", r.SkippedLines)
	}

	return b.String()
}

// StreamSummary counts the results seen by Stream. Failed counts failed
// tests; Packages counts packages that failed or did not build, which
// includes packages whose failure is already explained by a failed test.
type StreamSummary struct {
	failedPackages map[string]bool

	Passed   int
	Failed   int
	Skipped  int
	Packages int
}

// Stream copies the human-readable test output carried by a `go test -json`
// stream to w as the events arrive, so a CI log reads like `go test -v`, and
// counts the results. Package and build failures count as failures. Lines
// that are not JSON are copied unchanged.
func Stream(r io.Reader, w io.Writer) (StreamSummary, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)

	var summary StreamSummary

	for scanner.Scan() {
		line := scanner.Bytes()

		var ev struct {
			event

			Output string `json:"Output"`
		}

		if trimmed := bytes.TrimSpace(line); len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(trimmed, &ev) != nil {
			if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
				return summary, fmt.Errorf("write output: %w", err)
			}

			continue
		}

		if ev.Output != "" {
			if _, err := io.WriteString(w, ev.Output); err != nil {
				return summary, fmt.Errorf("write output: %w", err)
			}
		}

		summary.count(ev.event)
	}

	if err := scanner.Err(); err != nil {
		return summary, fmt.Errorf("read test output: %w", err)
	}

	return summary, nil
}

// OK reports whether the stream contained no failures.
func (s *StreamSummary) OK() bool {
	return s.Failed == 0 && s.Packages == 0
}

func (s *StreamSummary) count(ev event) {
	switch {
	case ev.Test != "" && ev.Action == "pass":
		s.Passed++
	case ev.Test != "" && ev.Action == "fail":
		s.Failed++
	case ev.Test != "" && ev.Action == "skip":
		s.Skipped++
	case ev.Test == "" && ev.Action == "fail":
		s.packageFailed(ev.Package)
	case ev.Test == "" && ev.Action == "build-fail":
		// A build failure names the test binary ("pkg [pkg.test]") and is
		// followed by a fail event for the package itself.
		name, _, _ := strings.Cut(ev.ImportPath, " ")
		s.packageFailed(name)
	}
}

func (s *StreamSummary) packageFailed(name string) {
	if s.failedPackages == nil {
		s.failedPackages = map[string]bool{}
	}

	if !s.failedPackages[name] {
		s.failedPackages[name] = true
		s.Packages++
	}
}
