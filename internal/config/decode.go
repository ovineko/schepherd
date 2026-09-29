package config

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/interp"
	"github.com/ovineko/schepherd/internal/match"
	"github.com/ovineko/schepherd/internal/runner"
)

const maxProblems = 20

var (
	topKeys      = []string{"config_version", "extends", "workspace", "offline", "cache_dir", "catalog", "limits", "registries", "runner", "mappings", "schemas"}
	catalogKeys  = []string{"repository", "digest"}
	limitKeys    = []string{"max_manifest_bytes", "max_catalog_bytes", "max_payload_bytes", "max_schema_bytes", "max_catalog_entries"}
	registryKeys = []string{"plain_http", "ca_file", "credentials_file"}
	runnerKeys   = []string{"mode", "command", "args", "cwd", "timeout", "jobs", "fail_fast", "inherit_env", "max_args_bytes", "env"}
	mappingKeys  = []string{"file_match", "schema"}
	schemaKeys   = []string{"path", "file_match"}

	modes = []runner.Mode{runner.ModeBatch, runner.ModePerFile, runner.ModeStdin}

	pathPlaceholders = []string{interp.Workspace, interp.Cache}
	envPlaceholders  = []string{interp.Schema, interp.SchemaID, interp.SchemaRef, interp.File, interp.Workspace, interp.Cache}
	argPlaceholders  = append(slices.Clone(envPlaceholders), interp.Files)

	schemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]+:`)
	idRE     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,126}[a-z0-9])?$`)
)

type problem struct {
	msg  string
	node node
}

// decoder validates one parsed TOML document against the configuration
// model. go-toml matches struct fields case-insensitively, so the document is
// decoded into generic maps and every key is compared exactly here.
type decoder struct {
	index    map[string]int
	file     string
	lines    lineIndex
	problems []problem
}

func parseLayer(file string, data []byte) (*layer, error) {
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		if de, ok := errors.AsType[*toml.DecodeError](err); ok {
			line, col := de.Position()

			return nil, fault.New(fault.Usage, "%s:%d:%d: invalid TOML: %s", file, line, col, strings.TrimPrefix(de.Error(), "toml: "))
		}

		return nil, fault.Wrap(fault.Usage, err, "%s: invalid TOML", file)
	}

	d := &decoder{file: file, index: indexKeys(data), lines: newLineIndex(data)}
	ly := d.document(doc)

	if err := d.err(); err != nil {
		return nil, err
	}

	return ly, nil
}

func (d *decoder) child(parent node, key string) node {
	path := joinKey(parent.path, key)
	if off, ok := d.index[path]; ok {
		return node{path: path, off: off}
	}

	return node{path: path, off: parent.off}
}

func (d *decoder) elem(parent node, i int) node {
	path := joinIndex(parent.path, i)
	if off, ok := d.index[path]; ok {
		return node{path: path, off: off}
	}

	return node{path: path, off: parent.off}
}

func (d *decoder) origin(n node) origin {
	o := origin{file: d.file, key: n.path}
	if n.off >= 0 {
		o.line, o.col = d.lines.position(n.off)
	}

	return o
}

func (d *decoder) failf(n node, format string, args ...any) {
	d.problems = append(d.problems, problem{node: n, msg: fmt.Sprintf(format, args...)})
}

func (d *decoder) err() error {
	if len(d.problems) == 0 {
		return nil
	}

	slices.SortStableFunc(d.problems, func(a, b problem) int { return cmp.Compare(a.node.off, b.node.off) })

	lines := make([]string, 0, min(len(d.problems), maxProblems+1))
	for i, p := range d.problems {
		if i == maxProblems {
			lines = append(lines, fmt.Sprintf("%s: %d more problems", d.file, len(d.problems)-maxProblems))

			break
		}

		lines = append(lines, d.origin(p.node).String()+": "+p.msg)
	}

	return fault.New(fault.Usage, "%s", strings.Join(lines, "\n"))
}

func (d *decoder) document(doc map[string]any) *layer {
	root := node{off: -1}
	ly := newLayer(d.file)

	if !d.version(root, doc) {
		return ly
	}

	d.known(root, doc, topKeys)
	ly.extends = d.extends(root, doc)
	ly.workspace = d.fixedPath(root, doc, "workspace")
	ly.cacheDir = d.fixedPath(root, doc, "cache_dir")
	ly.offline, _ = get[bool](d, root, doc, "offline")
	d.catalog(root, doc, ly)
	d.limits(root, doc, ly)
	d.registries(root, doc, ly)
	d.runner(root, doc, &ly.runner)
	ly.mappings = d.mappings(root, doc)
	d.schemas(root, doc, ly)

	return ly
}

// version reports whether the rest of the document can be interpreted with
// the version 1 model; a document of another version is not checked further
// so that its unknown keys do not bury the actual problem.
func (d *decoder) version(root node, doc map[string]any) bool {
	n := d.child(root, "config_version")
	if _, ok := doc["config_version"]; !ok {
		d.failf(n, "required key is missing; add config_version = %d", Version)

		return true
	}

	v, _ := get[int64](d, root, doc, "config_version")
	if !v.set {
		return false
	}

	if v.val != Version {
		d.failf(n, "unsupported configuration version %d (supported: %d)", v.val, Version)

		return false
	}

	return true
}

func (d *decoder) known(n node, m map[string]any, allowed []string) {
	for _, key := range slices.Sorted(maps.Keys(m)) {
		if slices.Contains(allowed, key) {
			continue
		}

		kn := d.child(n, key)

		if i := slices.IndexFunc(allowed, func(a string) bool { return strings.EqualFold(a, key) }); i >= 0 {
			d.failf(kn, "unknown key (keys are case-sensitive; did you mean %q?)", allowed[i])

			continue
		}

		d.failf(kn, "unknown key")
	}
}

func get[T any](d *decoder, parent node, m map[string]any, key string) (opt[T], node) {
	n := d.child(parent, key)

	raw, ok := m[key]
	if !ok {
		return opt[T]{}, n
	}

	v, ok := raw.(T)
	if !ok {
		var want T

		d.failf(n, "expected %s, got %s", typeName(want), typeName(raw))

		return opt[T]{}, n
	}

	return opt[T]{val: v, at: d.origin(n), set: true}, n
}

func (d *decoder) table(parent node, m map[string]any, key string) (node, map[string]any, bool) {
	v, n := get[map[string]any](d, parent, m, key)

	return n, v.val, v.set
}

func (d *decoder) nonEmpty(parent node, m map[string]any, key string) (opt[string], node) {
	v, n := get[string](d, parent, m, key)
	if v.set && v.val == "" {
		d.failf(n, "must not be empty")

		return opt[string]{}, n
	}

	return v, n
}

func (d *decoder) stringList(parent node, m map[string]any, key string) (opt[[]string], []node) {
	v, n := get[[]any](d, parent, m, key)
	if !v.set {
		return opt[[]string]{}, nil
	}

	list := make([]string, 0, len(v.val))
	nodes := make([]node, 0, len(v.val))
	ok := true

	for i, item := range v.val {
		en := d.elem(n, i)

		s, isString := item.(string)
		if !isString {
			d.failf(en, "expected a string, got %s", typeName(item))

			ok = false

			continue
		}

		list = append(list, s)
		nodes = append(nodes, en)
	}

	if !ok {
		return opt[[]string]{}, nil
	}

	return opt[[]string]{val: list, at: v.at, set: true}, nodes
}

func (d *decoder) template(n node, raw string, allowed []string) (*interp.Template, bool) {
	if strings.ContainsRune(raw, 0) {
		d.failf(n, "must not contain NUL characters")

		return nil, false
	}

	tpl, err := interp.Parse(raw)
	if err != nil {
		d.failf(n, "%v", err)

		return nil, false
	}

	for _, name := range tpl.Placeholders() {
		if slices.Contains(allowed, name) {
			continue
		}

		if len(allowed) == 0 {
			d.failf(n, "placeholder {%s} is not allowed here (only ${NAME} is expanded in this field; write {{ and }} for literal braces)", name)
		} else {
			d.failf(n, "placeholder {%s} is not allowed here (allowed: {%s})", name, strings.Join(allowed, "}, {"))
		}

		return nil, false
	}

	return tpl, true
}

func (d *decoder) templated(parent node, m map[string]any, key string, allowed []string) (opt[string], *interp.Template) {
	v, n := d.nonEmpty(parent, m, key)
	if !v.set {
		return v, nil
	}

	tpl, ok := d.template(n, v.val, allowed)
	if !ok {
		return opt[string]{}, nil
	}

	return v, tpl
}

func (d *decoder) fixedPath(parent node, m map[string]any, key string) opt[string] {
	v, _ := d.templated(parent, m, key, nil)

	return v
}

// checked reads a field without runtime placeholders and applies check to it
// when its value does not depend on the environment; otherwise the check runs
// on the expanded value at load time.
func (d *decoder) checked(parent node, m map[string]any, key string, check func(string) error) opt[string] {
	v, tpl := d.templated(parent, m, key, nil)
	if !v.set {
		return v
	}

	if len(tpl.EnvVars()) > 0 {
		return v
	}

	literal, err := tpl.Expand(unsetLookup, nil)
	if err == nil {
		err = check(literal)
	}

	if err != nil {
		d.failf(d.child(parent, key), "%v", err)

		return opt[string]{}
	}

	return v
}

func (d *decoder) positive(parent node, m map[string]any, key string, limit int64) opt[int64] {
	v, n := get[int64](d, parent, m, key)
	if v.set && (v.val < 1 || v.val > limit) {
		d.failf(n, "must be between 1 and %d, got %d", limit, v.val)

		return opt[int64]{}
	}

	return v
}

func (d *decoder) extends(root node, doc map[string]any) []extendsRef {
	list, nodes := d.stringList(root, doc, "extends")
	if !list.set {
		return nil
	}

	refs := make([]extendsRef, 0, len(list.val))

	for i, entry := range list.val {
		switch {
		case entry == "":
			d.failf(nodes[i], "must not be empty")
		case strings.ContainsRune(entry, 0):
			d.failf(nodes[i], "must not contain NUL characters")
		case schemeRE.MatchString(entry):
			d.failf(nodes[i], "%q is not a local file; only local files can be extended (write ./%s for a file name containing a colon)", entry, entry)
		default:
			refs = append(refs, extendsRef{path: entry, at: d.origin(nodes[i])})
		}
	}

	return refs
}

func (d *decoder) catalog(root node, doc map[string]any, ly *layer) {
	n, m, ok := d.table(root, doc, "catalog")
	if !ok {
		return
	}

	d.known(n, m, catalogKeys)
	ly.repository = d.checked(n, m, "repository", checkRepository)
	ly.digest = d.checked(n, m, "digest", checkDigest)
}

func (d *decoder) limits(root node, doc map[string]any, ly *layer) {
	n, m, ok := d.table(root, doc, "limits")
	if !ok {
		return
	}

	d.known(n, m, limitKeys)
	ly.limits.manifest = d.positive(n, m, "max_manifest_bytes", maxLimitBytes)
	ly.limits.catalog = d.positive(n, m, "max_catalog_bytes", maxLimitBytes)
	ly.limits.payload = d.positive(n, m, "max_payload_bytes", maxLimitBytes)
	ly.limits.schema = d.positive(n, m, "max_schema_bytes", maxLimitBytes)
	ly.limits.entries = d.positive(n, m, "max_catalog_entries", maxLimitEntries)
}

func (d *decoder) registries(root node, doc map[string]any, ly *layer) {
	n, m, ok := d.table(root, doc, "registries")
	if !ok {
		return
	}

	for _, key := range slices.Sorted(maps.Keys(m)) {
		kn, t, ok := d.table(n, m, key)
		if !ok {
			continue
		}

		if err := checkRegistryKey(key); err != nil {
			d.failf(kn, "%v", err)

			continue
		}

		d.known(kn, t, registryKeys)

		reg := &registryLayer{}
		reg.plainHTTP, _ = get[bool](d, kn, t, "plain_http")
		reg.caFile = d.fixedPath(kn, t, "ca_file")
		reg.credentialsFile = d.fixedPath(kn, t, "credentials_file")
		ly.registries[key] = reg
	}
}

func (d *decoder) runner(root node, doc map[string]any, r *runnerLayer) {
	n, m, ok := d.table(root, doc, "runner")
	if !ok {
		return
	}

	d.known(n, m, runnerKeys)

	if v, vn := d.nonEmpty(n, m, "mode"); v.set {
		if slices.Contains(modes, runner.Mode(v.val)) {
			r.mode = v
		} else {
			d.failf(vn, "must be one of batch, per-file, stdin, got %q", v.val)
		}
	}

	r.command, _ = d.templated(n, m, "command", pathPlaceholders)
	r.cwd, _ = d.templated(n, m, "cwd", pathPlaceholders)
	r.args = d.args(n, m)
	r.timeout = d.duration(n, m, "timeout")
	r.jobs = d.positive(n, m, "jobs", runner.MaxJobs)
	r.maxArgsBytes = d.positive(n, m, "max_args_bytes", maxLimitBytes)
	r.failFast, _ = get[bool](d, n, m, "fail_fast")
	r.inheritEnv, _ = get[bool](d, n, m, "inherit_env")
	d.runnerEnv(n, m, r)
}

func (d *decoder) args(parent node, m map[string]any) opt[[]string] {
	list, nodes := d.stringList(parent, m, "args")
	if !list.set {
		return list
	}

	ok := true

	for i, arg := range list.val {
		tpl, valid := d.template(nodes[i], arg, argPlaceholders)
		if !valid {
			ok = false

			continue
		}

		if tpl.Uses(interp.Files) && !tpl.IsFilesList() {
			d.failf(nodes[i], "{files...} must be a whole argument")

			ok = false
		}
	}

	if !ok {
		return opt[[]string]{}
	}

	return list
}

func (d *decoder) duration(parent node, m map[string]any, key string) opt[time.Duration] {
	v, n := get[string](d, parent, m, key)
	if !v.set {
		return opt[time.Duration]{}
	}

	value, err := time.ParseDuration(v.val)
	if err != nil || value <= 0 {
		d.failf(n, "must be a positive duration such as \"90s\" or \"5m\", got %q", v.val)

		return opt[time.Duration]{}
	}

	return opt[time.Duration]{val: value, at: v.at, set: true}
}

func (d *decoder) runnerEnv(parent node, m map[string]any, r *runnerLayer) {
	n, t, ok := d.table(parent, m, "env")
	if !ok {
		return
	}

	for _, name := range slices.Sorted(maps.Keys(t)) {
		v, vn := get[string](d, n, t, name)
		if !v.set {
			continue
		}

		if name == "" || strings.ContainsAny(name, "=\x00") {
			d.failf(vn, "invalid environment variable name %q", name)

			continue
		}

		if _, valid := d.template(vn, v.val, envPlaceholders); valid {
			r.env[name] = v
		}
	}
}

func (d *decoder) mappings(root node, doc map[string]any) opt[[]Mapping] {
	v, n := get[[]any](d, root, doc, "mappings")
	if !v.set {
		return opt[[]Mapping]{}
	}

	out := make([]Mapping, 0, len(v.val))
	before := len(d.problems)

	for i, item := range v.val {
		en := d.elem(n, i)

		t, ok := item.(map[string]any)
		if !ok {
			d.failf(en, "expected a table, got %s", typeName(item))

			continue
		}

		out = append(out, d.mapping(en, t))
	}

	if len(d.problems) > before {
		return opt[[]Mapping]{}
	}

	return opt[[]Mapping]{val: out, at: v.at, set: true}
}

func (d *decoder) mapping(n node, t map[string]any) Mapping {
	d.known(n, t, mappingKeys)

	var out Mapping

	if _, ok := t["schema"]; !ok {
		d.failf(d.child(n, "schema"), "required key is missing")
	} else if id, idn := get[string](d, n, t, "schema"); id.set {
		if err := checkSchemaID(id.val); err != nil {
			d.failf(idn, "%v", err)
		}

		out.Schema = id.val
	}

	if _, ok := t["file_match"]; !ok {
		d.failf(d.child(n, "file_match"), "required key is missing")

		return out
	}

	out.FileMatch = d.fileMatch(n, t).val

	return out
}

func (d *decoder) fileMatch(n node, t map[string]any) opt[[]string] {
	patterns, nodes := d.stringList(n, t, "file_match")
	if !patterns.set {
		return patterns
	}

	if len(patterns.val) == 0 {
		d.failf(d.child(n, "file_match"), "must list at least one pattern")
	}

	for i, p := range patterns.val {
		if err := match.ValidatePattern(p); err != nil {
			d.failf(nodes[i], "%v", err)
		}
	}

	return patterns
}

func (d *decoder) schemas(root node, doc map[string]any, ly *layer) {
	n, m, ok := d.table(root, doc, "schemas")
	if !ok {
		return
	}

	for _, id := range slices.Sorted(maps.Keys(m)) {
		kn, t, ok := d.table(n, m, id)
		if !ok {
			continue
		}

		if err := checkSchemaID(id); err != nil {
			d.failf(kn, "%v", err)

			continue
		}

		d.known(kn, t, schemaKeys)

		ly.schemas[id] = &localSchemaLayer{
			path:      d.checked(kn, t, "path", checkLocalPath),
			fileMatch: d.fileMatch(kn, t),
			decl:      d.origin(d.child(kn, "path")),
		}
	}
}

func checkSchemaID(id string) error {
	if !idRE.MatchString(id) || strings.Contains(id, "..") {
		return fmt.Errorf("schema id %q must match %s and must not contain \"..\"", id, idRE)
	}

	return nil
}

// checkLocalPath refuses URLs: a local schema is read from disk, and the
// client never fetches schemas from anywhere but its registries.
func checkLocalPath(p string) error {
	if schemeRE.MatchString(p) {
		return fmt.Errorf("%q is not a local file; local schemas are only read from disk (write ./%s for a file name containing a colon)", p, p)
	}

	return nil
}

func typeName(v any) string {
	switch v.(type) {
	case string:
		return "a string"
	case int64:
		return "an integer"
	case float64:
		return "a float"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	case map[string]any:
		return "a table"
	case toml.LocalDate, toml.LocalTime, toml.LocalDateTime, time.Time:
		return "a date or time"
	default:
		return fmt.Sprintf("a value of type %T", v)
	}
}

func unsetLookup(string) (string, bool) {
	return "", false
}

var bareKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// joinKey renders a key path the way it would be written in TOML, quoting
// segments that are not bare keys, so every path has exactly one spelling.
func joinKey(parent, key string) string {
	if !bareKeyRE.MatchString(key) {
		key = strconv.Quote(key)
	}

	if parent == "" {
		return key
	}

	return parent + "." + key
}

func joinIndex(parent string, i int) string {
	return parent + "[" + strconv.Itoa(i) + "]"
}
