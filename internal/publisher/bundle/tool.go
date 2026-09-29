package bundle

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/fault"
)

// PinnedVersion is the only Sourcemeta JSON Schema CLI release the publisher
// accepts. It must equal the version in tools/pins/jsonschema.json, from which
// `go run ./tools/install-jsonschema` installs the binary.
const PinnedVersion = "16.12.0"

const (
	versionTimeout = 30 * time.Second
	// runTimeout bounds one CLI run so that Resolve and Prepare finish even
	// without a caller deadline. Analysis time grows faster than linearly:
	// about 9 s for a 1.5 MB schema, several minutes at 15 MB.
	runTimeout = 10 * time.Minute
	// addressSpaceFloor and addressSpaceFactor bound the virtual memory of one
	// CLI run (Linux only); about 1 GiB was measured for a 15 MB schema, and
	// the floor keeps ordinary runs far away from the limit.
	addressSpaceFloor  = 4 << 30
	addressSpaceFactor = 256
	waitDelay          = 5 * time.Second
	stderrLimit        = 64 << 10
	versionLimit       = 4 << 10
	errorJSONLimit     = 1 << 20
	// inspectFactor bounds `inspect --json` output relative to its input; the
	// report lists every subschema location with positions and is typically
	// about twenty times the size of the schema.
	inspectFactor  = 64
	inspectMinimum = 64 << 20
)

// Tool is a verified installation of the Sourcemeta JSON Schema CLI. The CLI
// is AGPL-3.0 licensed maintainer tooling: it is only ever executed as a
// separate process, never linked into or shipped with Schepherd.
type Tool struct {
	Path    string
	Version string

	runTimeout   time.Duration
	addressSpace uint64
}

// Ref is a reference that leaves the document it appears in.
type Ref struct {
	// Origin is the JSON Pointer of the referencing keyword.
	Origin string
	// Destination is the absolute target URI including any fragment.
	Destination string
	// Base is Destination without its fragment: the resource to obtain.
	Base string
}

// FindTool locates the CLI at path (an executable name looked up in PATH, or
// a file path) and requires `<path> --version` to report exactly wantVersion.
// An empty wantVersion means PinnedVersion. A missing or mismatching CLI is a
// fault.Usage error: the maintainer has to install the pinned release.
func FindTool(path string, wantVersion string) (*Tool, error) {
	if wantVersion == "" {
		wantVersion = PinnedVersion
	}

	if path == "" {
		path = "jsonschema"
	}

	resolved, err := exec.LookPath(path)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "JSON Schema CLI %q not found (install it with 'go run ./tools/install-jsonschema')", path)
	}

	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "resolve JSON Schema CLI path")
	}

	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()

	stdout := &cappedBuffer{limit: versionLimit}
	stderr := &cappedBuffer{limit: versionLimit}

	// bearer:disable go_gosec_injection_subproc_injection
	// Running the maintainer-configured CLI is the purpose; no shell is involved.
	cmd := exec.CommandContext(ctx, resolved, "--version") //nolint:gosec // G204: running the maintainer-configured CLI is the purpose; no shell is involved
	cmd.Env = env.WithoutGitHubToken(env.Environ())
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay

	if err := cmd.Run(); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "run %s --version (%s)", resolved, strings.TrimSpace(stderr.String()))
	}

	version := strings.TrimSpace(stdout.String())
	if version != wantVersion {
		return nil, fault.New(fault.Usage,
			"JSON Schema CLI %s reports version %q, want exactly %q (install it with 'go run ./tools/install-jsonschema')",
			resolved, version, wantVersion)
	}

	return &Tool{Path: resolved, Version: version}, nil
}

// workspace is a private temporary directory holding the files one CLI
// session reads, plus an empty configuration file. Passing that file with -C
// stops the CLI from picking up a jsonschema.json from any parent directory.
type workspace struct {
	files  map[string]string
	dir    string
	config string
	seq    int
}

func newWorkspace() (*workspace, error) {
	dir, err := os.MkdirTemp("", "schepherd-bundle-*")
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "create bundler workspace")
	}

	ws := &workspace{dir: dir, config: filepath.Join(dir, "jsonschema.json"), files: map[string]string{}}

	if err := os.WriteFile(ws.config, []byte("{}\n"), 0o600); err != nil {
		ws.close()

		return nil, fault.Wrap(fault.Internal, err, "write bundler configuration")
	}

	return ws, nil
}

func (ws *workspace) close() {
	_ = os.RemoveAll(ws.dir)
}

// write stores data as the next numbered document and remembers which URI
// it stands for, so CLI messages can name the document instead of the path.
func (ws *workspace) write(uri string, data []byte) (string, error) {
	ws.seq++
	name := filepath.Join(ws.dir, fmt.Sprintf("%04d.json", ws.seq))

	if err := os.WriteFile(name, data, 0o600); err != nil {
		return "", fault.Wrap(fault.Internal, err, "write bundler input")
	}

	ws.files[name] = uri

	return name, nil
}

func (ws *workspace) output(prefix string) (*os.File, error) {
	file, err := os.CreateTemp(ws.dir, prefix+"-*.out")
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "create bundler output file")
	}

	return file, nil
}

// sanitize replaces workspace paths in a CLI message with the URIs of the
// documents they hold so details never leak local temporary paths.
func (ws *workspace) sanitize(msg string) string {
	for path, uri := range ws.files {
		msg = strings.ReplaceAll(msg, fileURI(path), uri)
		msg = strings.ReplaceAll(msg, path, uri)
	}

	msg = strings.ReplaceAll(msg, fileURI(ws.dir), "<workspace>")

	return strings.ReplaceAll(msg, ws.dir, "<workspace>")
}

func fileURI(path string) string {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}

	return "file://" + slashed
}

type runResult struct {
	stdout string
	stderr string
	exit   int
}

func (t *Tool) run(ctx context.Context, ws *workspace, args ...string) (runResult, error) {
	out, err := ws.output(args[0])
	if err != nil {
		return runResult{}, err
	}

	timeout := cmp.Or(t.runTimeout, runTimeout)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stderr := &cappedBuffer{limit: stderrLimit}

	cmd := exec.CommandContext(runCtx, t.Path, args...) //nolint:gosec // G204: version-verified CLI; arguments are fixed flags and workspace paths, no shell
	cmd.Env = env.WithoutGitHubToken(env.Environ())
	cmd.Dir = ws.dir
	cmd.Stdout = out
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay

	runErr := startLimited(cmd, t.addressSpaceFor(ws.inputSize(args)))
	if runErr == nil {
		runErr = cmd.Wait()
	}

	closeErr := out.Close()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return runResult{}, fault.Wrap(fault.KindOf(ctxErr), ctxErr, "JSON Schema CLI %s interrupted", args[0])
	}

	if runCtx.Err() != nil {
		return runResult{}, reject(ReasonBundlerError, "JSON Schema CLI %s did not finish within %s", args[0], timeout)
	}

	result := runResult{stdout: out.Name(), stderr: stderr.String()}

	if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
		result.exit = exitErr.ExitCode()
	} else if runErr != nil {
		return runResult{}, fault.Wrap(fault.Internal, runErr, "run JSON Schema CLI %s", args[0])
	}

	if closeErr != nil {
		return runResult{}, fault.Wrap(fault.Internal, closeErr, "write JSON Schema CLI output")
	}

	return result, nil
}

func startLimited(cmd *exec.Cmd, addressSpace uint64) error {
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	if err := limitResources(cmd.Process.Pid, addressSpace); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		return err
	}

	return nil
}

func (t *Tool) addressSpaceFor(inputBytes int64) uint64 {
	if t.addressSpace != 0 {
		return t.addressSpace
	}

	return max(addressSpaceFloor, addressSpaceFactor*uint64(max(inputBytes, 0)))
}

func (ws *workspace) inputSize(args []string) int64 {
	var total int64

	for _, arg := range args {
		if _, ok := ws.files[arg]; !ok {
			continue
		}

		if info, err := os.Stat(arg); err == nil {
			total += info.Size()
		}
	}

	return total
}

// defaultDialect applies to documents that do not declare $schema.
func (t *Tool) inspect(ctx context.Context, ws *workspace, file, defaultDialect string) (*inspection, error) {
	args := []string{"inspect", file, "--json", "-C", ws.config}
	if defaultDialect != "" {
		args = append(args, "-d", defaultDialect)
	}

	res, err := t.run(ctx, ws, args...)
	if err != nil {
		return nil, err
	}

	if res.exit != 0 {
		return nil, ws.cliFailure(res)
	}

	input, err := os.Stat(file)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "stat inspected file")
	}

	output, err := os.Stat(res.stdout)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "stat inspect output")
	}

	if limit := max(inspectMinimum, inspectFactor*input.Size()); output.Size() > limit {
		return nil, reject(ReasonDependencyLimit, "inspect report of %d bytes for %s exceeds the limit of %d bytes", output.Size(), ws.files[file], limit)
	}

	f, err := os.Open(res.stdout)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "read inspect output")
	}

	defer func() { _ = f.Close() }()

	// A report that cannot be read fails this document only, as malformed
	// bundle output does: one odd upstream schema must not stop a whole
	// preparation, where a published schema is held instead.
	in, err := parseInspection(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return nil, rejectWrap(ReasonBundlerError, err, "JSON Schema CLI inspect report for %s", ws.files[file])
	}

	return in, nil
}

// bundle runs the bundler on root with deps imported by --resolve and
// returns its output, read up to limit bytes. The run never enables --http.
func (t *Tool) bundle(ctx context.Context, ws *workspace, root string, deps []string, defaultDialect string, limit int64) ([]byte, error) {
	args := make([]string, 0, 2*len(deps)+9)
	args = append(args, "bundle", root)

	for _, dep := range deps {
		args = append(args, "-r", dep)
	}

	args = append(args, "--json", "-C", ws.config, "-d", defaultDialect)

	res, err := t.run(ctx, ws, args...)
	if err != nil {
		return nil, err
	}

	if res.exit != 0 {
		return nil, ws.cliFailure(res)
	}

	info, err := os.Stat(res.stdout)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "stat bundle output")
	}

	if info.Size() > limit {
		return nil, reject(ReasonDependencyLimit, "bundled output of %d bytes exceeds the limit of %d bytes", info.Size(), limit)
	}

	out, err := os.ReadFile(res.stdout)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "read bundle output")
	}

	return out, nil
}

type cliError struct {
	Error      string `json:"error"`
	Identifier string `json:"identifier"`
	FilePath   string `json:"filePath"`
}

// cliFailure turns a failed CLI run into a Failure. With --json the CLI
// reports errors as a JSON object on stdout; anything else falls back to
// stderr.
func (ws *workspace) cliFailure(res runResult) error {
	if res.exit == 5 {
		return fault.New(fault.Internal, "JSON Schema CLI rejected its arguments: %s", ws.sanitize(strings.TrimSpace(res.stderr)))
	}

	var report cliError

	raw, _ := readPrefix(res.stdout, errorJSONLimit)
	if err := json.Unmarshal(raw, &report); err != nil || report.Error == "" {
		msg := strings.TrimSpace(res.stderr)
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}

		return reject(ReasonBundlerError, "JSON Schema CLI exited with status %d: %s", res.exit, ws.sanitize(msg))
	}

	detail := report.Error
	if report.Identifier != "" {
		detail += " " + strconv.Quote(report.Identifier)
	}

	if report.FilePath != "" {
		detail += " in " + report.FilePath
	}

	return reject(classifyCLIError(report.Error), "%s", ws.sanitize(detail))
}

func classifyCLIError(msg string) string {
	switch {
	case strings.HasPrefix(msg, "Could not resolve the reference to an external schema"),
		strings.HasPrefix(msg, "Could not resolve schema reference"):
		return ReasonUnresolvedRef
	case strings.HasPrefix(msg, "Could not resolve the metaschema of the schema"),
		strings.HasPrefix(msg, "Relative meta-schema URIs are not valid"):
		return ReasonUnsupportedDialect
	case strings.HasPrefix(msg, "Could not determine the base dialect of the schema"):
		return ReasonUndeclaredDialect
	case strings.Contains(msg, "top-level `$ref`"):
		return ReasonTopLevelRefDraft7
	case strings.HasPrefix(msg, "Conflicting schemas for the same identifier"):
		return ReasonIDMismatch
	default:
		return ReasonBundlerError
	}
}

func readPrefix(name string, limit int64) ([]byte, error) {
	f, err := os.Open(filepath.Clean(name))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}

	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}

	return data, nil
}

type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}

	return len(p), nil
}

func (c *cappedBuffer) String() string {
	return c.buf.String()
}

type inspectedRef struct {
	Origin      string `json:"origin"`
	Original    string `json:"original"`
	Destination string `json:"destination"`
	Base        string `json:"base"`
}

// inspection is the part of `inspect --json` the bundler needs: the resource
// identifiers declared in the document and every reference it contains.
type inspection struct {
	resources map[string]struct{}
	refs      []inspectedRef
}

func (in *inspection) external() []Ref {
	var refs []Ref

	for _, r := range in.refs {
		if isSchemaKeyword(r.Origin) && isWellKnownMetaschema(r.Destination) {
			continue
		}

		if _, ok := in.resources[r.Base]; ok {
			continue
		}

		refs = append(refs, Ref{Origin: r.Origin, Destination: r.Destination, Base: r.Base})
	}

	return refs
}

func isSchemaKeyword(origin string) bool {
	return origin == "/$schema" || strings.HasSuffix(origin, "/$schema")
}

// parseInspection streams the report instead of decoding it whole: for large
// schemas the location table is two orders of magnitude bigger than the
// resources and references kept here.
func parseInspection(r io.Reader) (*inspection, error) {
	dec := json.NewDecoder(r)
	in := &inspection{resources: map[string]struct{}{}}

	err := walkObject(dec, func(key string) error {
		switch key {
		case "locations":
			return walkObject(dec, func(kind string) error {
				if kind != "static" {
					return skipValue(dec)
				}

				return walkObject(dec, func(uri string) error {
					var location struct {
						Type string `json:"type"`
					}

					if err := dec.Decode(&location); err != nil {
						return fmt.Errorf("location %q: %w", uri, err)
					}

					if location.Type == "resource" {
						in.resources[uri] = struct{}{}
					}

					return nil
				})
			})
		case "references":
			if err := dec.Decode(&in.refs); err != nil {
				return fmt.Errorf("references: %w", err)
			}

			return nil
		default:
			return skipValue(dec)
		}
	})
	if err != nil {
		return nil, err
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the inspect report")
	}

	return in, nil
}

func walkObject(dec *json.Decoder, member func(key string) error) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("read object: %w", err)
	}

	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("expected an object, got %v", tok)
	}

	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("read member name: %w", err)
		}

		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("expected a member name, got %v", tok)
		}

		if err := member(key); err != nil {
			return err
		}
	}

	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("read object end: %w", err)
	}

	return nil
}

func skipValue(dec *json.Decoder) error {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("skip value: %w", err)
	}

	return nil
}
