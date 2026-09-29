package runner

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/interp"
)

// DefaultKillGrace is how long a consumer group gets between SIGTERM and
// SIGKILL when it is stopped on timeout or interruption.
const DefaultKillGrace = 2 * time.Second

// Input is one file handed to the consumer together with the schema it was
// resolved to. Path must be absolute.
type Input struct {
	Path     string
	SchemaID string
}

// SchemaInfo describes a schema ready for a consumer. For a catalog schema
// Path is the materialized schema.json in the cache and Ref is
// "repository@<manifest digest>"; for a local schema Path is the project's
// own file and Ref is "local:" followed by its path. Origin is where the
// schema comes from ("catalog" or "local") and is copied into the report.
type SchemaInfo struct {
	ID     string
	Path   string
	Ref    string
	Origin string
}

// Options is the environment of one run. Workspace is absolute. CacheDir is
// absolute, or empty when no cache directory is available and Spec never uses
// {cache}: a run on local schemas alone needs no cache.
// Lookup resolves ${NAME} in templates from Schepherd's own environment;
// Environ is the base environment of every consumer when Spec.InheritEnv is
// set. Nil writers discard the consumer's output.
type Options struct {
	Lookup    interp.LookupFunc
	Stdout    io.Writer
	Stderr    io.Writer
	Workspace string
	CacheDir  string
	Environ   []string
	Spec      Spec
	KillGrace time.Duration
}

// Task is one consumer process, fully expanded. Path is the resolved
// absolute executable and Args the complete argument vector with Args[0] set
// to the command as configured. StdinFile is the input streamed to the
// consumer in stdin mode.
type Task struct {
	SchemaID     string
	SchemaRef    string
	SchemaOrigin string
	Path         string
	Dir          string
	StdinFile    string
	Files        []string
	Args         []string
	Env          []string
	Index        int
}

// Plan is the ordered list of consumer processes of one run.
type Plan struct {
	Mode  Mode
	Tasks []Task
}

type group struct {
	schema SchemaInfo
	files  []string
}

type templates struct {
	command *interp.Template
	cwd     *interp.Template
	args    []*interp.Template
	envKeys []string
	env     []*interp.Template
}

type planner struct {
	lookup      interp.LookupFunc
	executables map[string]string
	tpl         templates
	opts        Options
	command     string
	dir         string
	spec        Spec
}

type envVar struct {
	key   string
	value string
}

// BuildPlan validates the runner configuration, groups the inputs into
// consumer processes and expands every template. Duplicate input paths are
// dropped after their first occurrence. In batch mode one task is created per
// schema ID in order of first appearance, split into several tasks when the
// argument vector would exceed Spec.MaxArgsBytes; the other modes create one
// task per input. Nothing is started: every error surfaces before the first
// consumer runs.
func BuildPlan(inputs []Input, schemas map[string]SchemaInfo, opts Options) (*Plan, error) {
	spec, err := effectiveSpec(opts.Spec)
	if err != nil {
		return nil, err
	}

	if err := checkRoots(opts); err != nil {
		return nil, err
	}

	groups, err := groupInputs(inputs, schemas, spec.Mode)
	if err != nil {
		return nil, err
	}

	tpl, err := parseTemplates(spec)
	if err != nil {
		return nil, err
	}

	p := &planner{spec: spec, opts: opts, tpl: tpl, lookup: opts.Lookup, executables: map[string]string{}}
	if p.lookup == nil {
		p.lookup = func(string) (string, bool) { return "", false }
	}

	if err := p.resolveFixed(); err != nil {
		return nil, err
	}

	plan := &Plan{Mode: spec.Mode, Tasks: []Task{}}

	for _, g := range groups {
		tasks, err := p.tasksFor(g)
		if err != nil {
			return nil, err
		}

		plan.Tasks = append(plan.Tasks, tasks...)
	}

	for i := range plan.Tasks {
		plan.Tasks[i].Index = i
	}

	return plan, nil
}

func effectiveSpec(spec Spec) (Spec, error) {
	if spec.Cwd == "" {
		spec.Cwd = DefaultCwd
	}

	if spec.MaxArgsBytes == 0 {
		spec.MaxArgsBytes = DefaultMaxArgsBytes()
	}

	if err := ValidateSpec(spec); err != nil {
		return Spec{}, &fault.Error{Kind: fault.Usage, Err: err}
	}

	for _, key := range slices.Sorted(maps.Keys(spec.Env)) {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return Spec{}, fault.New(fault.Usage, "runner.env: invalid variable name %q", key)
		}
	}

	return spec, nil
}

func checkRoots(opts Options) error {
	if !filepath.IsAbs(opts.Workspace) {
		return fault.New(fault.Internal, "runner: workspace %q is not absolute", opts.Workspace)
	}

	switch {
	case opts.CacheDir == "" && opts.Spec.Uses(interp.Cache):
		return fault.New(fault.Internal, "runner: {cache} is used but no cache directory was given")
	case opts.CacheDir != "" && !filepath.IsAbs(opts.CacheDir):
		return fault.New(fault.Internal, "runner: cache directory %q is not absolute", opts.CacheDir)
	}

	return nil
}

func groupInputs(inputs []Input, schemas map[string]SchemaInfo, mode Mode) ([]group, error) {
	seen := make(map[string]struct{}, len(inputs))
	byID := map[string]int{}

	var groups []group

	for _, in := range inputs {
		path := filepath.Clean(in.Path)
		if !filepath.IsAbs(path) {
			return nil, fault.New(fault.Internal, "runner: input path %q is not absolute", in.Path)
		}

		if strings.IndexByte(path, 0) >= 0 {
			return nil, fault.New(fault.Usage, "input path %q contains a NUL byte", in.Path)
		}

		if _, dup := seen[path]; dup {
			continue
		}

		seen[path] = struct{}{}

		schema, ok := schemas[in.SchemaID]
		if !ok {
			return nil, fault.New(fault.Internal, "runner: schema %q of %s was not materialized", in.SchemaID, path)
		}

		schema.ID = in.SchemaID

		if mode != ModeBatch {
			groups = append(groups, group{schema: schema, files: []string{path}})

			continue
		}

		if i, ok := byID[in.SchemaID]; ok {
			groups[i].files = append(groups[i].files, path)

			continue
		}

		byID[in.SchemaID] = len(groups)
		groups = append(groups, group{schema: schema, files: []string{path}})
	}

	return groups, nil
}

func parseTemplates(spec Spec) (templates, error) {
	var (
		t   templates
		err error
	)

	if t.command, err = parse("runner.command", spec.Command); err != nil {
		return t, err
	}

	if t.cwd, err = parse("runner.cwd", spec.Cwd); err != nil {
		return t, err
	}

	for i, raw := range spec.Args {
		tpl, err := parse(fmt.Sprintf("runner.args[%d]", i), raw)
		if err != nil {
			return t, err
		}

		t.args = append(t.args, tpl)
	}

	t.envKeys = slices.Sorted(maps.Keys(spec.Env))
	for _, key := range t.envKeys {
		tpl, err := parse("runner.env."+key, spec.Env[key])
		if err != nil {
			return t, err
		}

		t.env = append(t.env, tpl)
	}

	return t, nil
}

func parse(field, raw string) (*interp.Template, error) {
	tpl, err := interp.Parse(raw)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "%s", field)
	}

	return tpl, nil
}

func (p *planner) resolveFixed() error {
	fixed := p.baseValues()

	command, err := p.expand("runner.command", p.tpl.command, fixed)
	if err != nil {
		return err
	}

	if command == "" {
		return fault.New(fault.Usage, "runner.command expands to an empty string")
	}

	dir, err := p.expand("runner.cwd", p.tpl.cwd, fixed)
	if err != nil {
		return err
	}

	if dir == "" {
		return fault.New(fault.Usage, "runner.cwd expands to an empty string")
	}

	p.command = command
	p.dir, err = resolveDir(dir, p.spec.CwdBase)

	return err
}

func (p *planner) baseValues() map[string]string {
	return map[string]string{
		interp.Workspace: p.opts.Workspace,
		interp.Cache:     p.opts.CacheDir,
	}
}

func (p *planner) values(schema SchemaInfo, file string) map[string]string {
	values := p.baseValues()
	values[interp.Schema] = schema.Path
	values[interp.SchemaID] = schema.ID
	values[interp.SchemaRef] = schema.Ref

	if file != "" {
		values[interp.File] = file
	}

	return values
}

func (p *planner) expand(field string, tpl *interp.Template, values map[string]string) (string, error) {
	s, err := tpl.Expand(p.lookup, values)
	if err != nil {
		return "", fault.Wrap(fault.Usage, err, "%s", field)
	}

	if strings.IndexByte(s, 0) >= 0 {
		return "", fault.New(fault.Usage, "%s expands to a value containing a NUL byte", field)
	}

	return s, nil
}

func (p *planner) tasksFor(g group) ([]Task, error) {
	file := ""
	if p.spec.Mode != ModeBatch {
		file = g.files[0]
	}

	values := p.values(g.schema, file)

	env, err := p.childEnv(values)
	if err != nil {
		return nil, err
	}

	path, err := p.executable(env)
	if err != nil {
		return nil, err
	}

	head, tail, err := p.args(values)
	if err != nil {
		return nil, err
	}

	chunks := [][]string{g.files}
	if p.spec.Mode == ModeBatch {
		chunks, err = chunkFiles(g.files, p.spec.MaxArgsBytes-argvBytes(head)-argvBytes(tail)-envBytes(env))
		if err != nil {
			return nil, fault.Wrap(fault.Usage, err, "schema %q", g.schema.ID)
		}
	}

	tasks := make([]Task, 0, len(chunks))

	for _, files := range chunks {
		task := Task{
			SchemaID:     g.schema.ID,
			SchemaRef:    g.schema.Ref,
			SchemaOrigin: g.schema.Origin,
			Path:         path,
			Dir:          p.dir,
			Files:        files,
			Env:          env,
		}

		switch p.spec.Mode {
		case ModeBatch:
			task.Args = slices.Concat(head, files, tail)
		case ModeStdin:
			task.Args = slices.Concat(head, tail)
			task.StdinFile = file
		case ModePerFile:
			task.Args = slices.Concat(head, tail)
		}

		tasks = append(tasks, task)
	}

	return tasks, nil
}

// args expands the argument templates. In batch mode head holds the command
// and everything before {files...} and tail everything after it; otherwise
// tail is empty.
func (p *planner) args(values map[string]string) (head, tail []string, err error) {
	head = []string{p.command}
	inTail := false

	for i, tpl := range p.tpl.args {
		if tpl.IsFilesList() {
			inTail = true

			continue
		}

		arg, err := p.expand(fmt.Sprintf("runner.args[%d]", i), tpl, values)
		if err != nil {
			return nil, nil, err
		}

		if inTail {
			tail = append(tail, arg)
		} else {
			head = append(head, arg)
		}
	}

	return head, tail, nil
}

func (p *planner) childEnv(values map[string]string) ([]string, error) {
	overlay := make([]envVar, 0, len(p.tpl.envKeys))

	for i, key := range p.tpl.envKeys {
		value, err := p.expand("runner.env."+key, p.tpl.env[i], values)
		if err != nil {
			return nil, err
		}

		overlay = append(overlay, envVar{key: key, value: value})
	}

	var base []string
	if p.spec.InheritEnv {
		base = p.opts.Environ
	}

	return mergeEnv(base, overlay), nil
}

// chunkFiles splits files greedily, in order, into runs whose argv cost stays
// within budget.
func chunkFiles(files []string, budget int) ([][]string, error) {
	var (
		chunks  [][]string
		current []string
		used    int
	)

	for _, f := range files {
		cost := argBytes(f)
		if cost > budget {
			return nil, fmt.Errorf(
				"file %s does not fit into runner.max_args_bytes (%d bytes left after the fixed arguments and environment); "+
					"use runner.mode = \"per-file\" or raise runner.max_args_bytes", f, max(budget, 0))
		}

		if len(current) > 0 && used+cost > budget {
			chunks = append(chunks, current)
			current, used = nil, 0
		}

		current = append(current, f)
		used += cost
	}

	if len(current) > 0 {
		chunks = append(chunks, current)
	}

	return chunks, nil
}

func argvBytes(args []string) int {
	total := 0
	for _, a := range args {
		total += argBytes(a)
	}

	return total
}

// mergeEnv overlays variables on base. Later duplicates of a key replace the
// earlier value in place, so the result has no duplicate keys and a stable
// order: base order first, then new keys in overlay order.
func mergeEnv(base []string, overlay []envVar) []string {
	out := make([]string, 0, len(base)+len(overlay))
	index := make(map[string]int, len(base)+len(overlay))

	put := func(key, entry string) {
		folded := foldEnvKey(key)
		if i, ok := index[folded]; ok {
			out[i] = entry

			return
		}

		index[folded] = len(out)
		out = append(out, entry)
	}

	for _, entry := range base {
		key, ok := envKey(entry)
		if !ok {
			continue
		}

		put(key, entry)
	}

	for _, v := range overlay {
		put(v.key, v.key+"="+v.value)
	}

	return out
}

// envKey returns the name of a KEY=value entry. A leading "=" belongs to the
// name, as in the per-drive "=C:" variables on Windows.
func envKey(entry string) (string, bool) {
	i := strings.IndexByte(entry, '=')
	if i == 0 {
		i = strings.IndexByte(entry[1:], '=') + 1
	}

	if i <= 0 {
		return "", false
	}

	return entry[:i], true
}

func lookupEnv(env []string, key string) (string, bool) {
	folded := foldEnvKey(key)

	for _, entry := range slices.Backward(env) {
		name, ok := envKey(entry)
		if ok && foldEnvKey(name) == folded {
			return entry[len(name)+1:], true
		}
	}

	return "", false
}

func resolveDir(dir, base string) (string, error) {
	if !filepath.IsAbs(dir) {
		if isDriveRelative(dir) {
			return "", fault.New(fault.Usage, "runner.cwd %q is relative to a drive; use an absolute path", dir)
		}

		if !filepath.IsAbs(base) {
			return "", fault.New(fault.Internal, "runner: relative runner.cwd %q has no absolute base directory", dir)
		}

		dir = filepath.Join(base, dir)
	}

	dir = filepath.Clean(dir)

	info, err := os.Stat(dir)
	if err != nil {
		return "", fault.Reclassify(fault.ConsumerStart, err, "runner.cwd %s is not usable", dir)
	}

	if !info.IsDir() {
		return "", fault.New(fault.ConsumerStart, "runner.cwd %s is not a directory", dir)
	}

	return dir, nil
}

func isDriveRelative(path string) bool {
	return filepath.VolumeName(path) != "" || (path != "" && os.IsPathSeparator(path[0]))
}
