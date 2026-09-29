package runner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/interp"
)

func TestBuildPlanBatchGroupsInFirstAppearanceOrder(t *testing.T) {
	h := newHarness(t)
	schemas := h.schemas("b", "a")
	a1, b1, a2, b2 := h.file("a1.json", ""), h.file("b1.json", ""), h.file("a2.json", ""), h.file("b2.json", "")
	inputs := []Input{
		{Path: b1, SchemaID: "b"},
		{Path: a1, SchemaID: "a"},
		{Path: b1, SchemaID: "b"},
		{Path: a2, SchemaID: "a"},
		{Path: filepath.Join(h.workspace, ".", "a1.json"), SchemaID: "a"},
		{Path: b2, SchemaID: "b"},
	}

	plan, err := BuildPlan(inputs, schemas, h.options(h.spec(ModeBatch, "validate", "{schema}", "{files...}", "--ref={schema-ref}")))
	if err != nil {
		t.Fatal(err)
	}

	if plan.Mode != ModeBatch || len(plan.Tasks) != 2 {
		t.Fatalf("plan = %+v, want two batch tasks", plan)
	}

	want := []struct {
		id    string
		files []string
	}{{"b", []string{b1, b2}}, {"a", []string{a1, a2}}}

	for i, w := range want {
		task := plan.Tasks[i]
		s := schemas[w.id]
		wantArgs := slices.Concat([]string{h.exe, "validate", s.Path}, w.files, []string{"--ref=" + s.Ref})

		if task.Index != i || task.SchemaID != w.id || task.SchemaRef != s.Ref {
			t.Errorf("task %d = %+v", i, task)
		}

		if !slices.Equal(task.Files, w.files) || !slices.Equal(task.Args, wantArgs) {
			t.Errorf("task %d files = %q args = %q, want %q / %q", i, task.Files, task.Args, w.files, wantArgs)
		}

		if task.Path != h.exe || task.Dir != h.workspace || task.StdinFile != "" {
			t.Errorf("task %d path/dir/stdin = %q %q %q", i, task.Path, task.Dir, task.StdinFile)
		}
	}
}

func TestBuildPlanChunksAtMaxArgsBytes(t *testing.T) {
	h := newHarness(t)
	schemas := h.schemas("a")

	inputs := make([]Input, 0, 5)

	for i := range 5 {
		inputs = append(inputs, Input{Path: h.file(string(rune('a'+i))+".json", ""), SchemaID: "a"})
	}

	spec := h.spec(ModeBatch, "check", "{schema}", "{files...}", "--end")
	spec.Env = map[string]string{"PADDING": strings.Repeat("x", 100)}
	opts := h.options(spec)
	env := mergeEnv(opts.Environ, []envVar{{key: "PADDING", value: strings.Repeat("x", 100)}})
	fixed := argvBytes([]string{h.exe, "check", schemas["a"].Path, "--end"}) + envBytes(env)
	perFile := argBytes(inputs[0].Path)

	cases := []struct {
		budget int
		sizes  []int
	}{
		{fixed + 5*perFile, []int{5}},
		{fixed + 2*perFile, []int{2, 2, 1}},
		{fixed + 2*perFile + perFile - 1, []int{2, 2, 1}},
		{fixed + perFile, []int{1, 1, 1, 1, 1}},
	}

	for _, c := range cases {
		opts.Spec.MaxArgsBytes = c.budget

		plan, err := BuildPlan(inputs, schemas, opts)
		if err != nil {
			t.Fatalf("budget %d: %v", c.budget, err)
		}

		sizes := make([]string, 0, len(plan.Tasks))
		seen := make([]string, 0, len(inputs))

		for _, task := range plan.Tasks {
			sizes = append(sizes, string(rune('0'+len(task.Files))))
			seen = append(seen, task.Files...)

			if got := argvBytes(task.Args) + envBytes(task.Env); got > c.budget {
				t.Errorf("budget %d: task uses %d bytes", c.budget, got)
			}

			if task.Args[len(task.Args)-1] != "--end" {
				t.Errorf("budget %d: args %q lost the argument after {files...}", c.budget, task.Args)
			}
		}

		var wantSizes []string
		for _, n := range c.sizes {
			wantSizes = append(wantSizes, string(rune('0'+n)))
		}

		if !slices.Equal(sizes, wantSizes) {
			t.Errorf("budget %d: chunk sizes %v, want %v", c.budget, sizes, wantSizes)
		}

		all := make([]string, 0, len(inputs))
		for _, in := range inputs {
			all = append(all, in.Path)
		}

		if !slices.Equal(seen, all) {
			t.Errorf("budget %d: files %q, want %q in input order", c.budget, seen, all)
		}
	}

	opts.Spec.MaxArgsBytes = fixed + perFile - 1

	_, err := BuildPlan(inputs, schemas, opts)
	wantKind(t, err, fault.Usage)

	if !strings.Contains(err.Error(), "per-file") {
		t.Errorf("error %q does not suggest per-file mode", err)
	}
}

func TestBuildPlanPerFileAndStdin(t *testing.T) {
	h := newHarness(t)
	schemas := h.schemas("a", "b")
	x, y := h.file("x.json", ""), h.file("dir/y.json", "")
	inputs := []Input{{Path: x, SchemaID: "a"}, {Path: y, SchemaID: "b"}, {Path: x, SchemaID: "a"}}

	for _, mode := range []Mode{ModePerFile, ModeStdin} {
		spec := h.spec(mode, "--schema={schema}", "{file}", "--id", "{schema-id}", "{workspace}", "{cache}")
		spec.Env["FILE"] = "{file}"

		plan, err := BuildPlan(inputs, schemas, h.options(spec))
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}

		if len(plan.Tasks) != 2 {
			t.Fatalf("%s: %d tasks, want 2", mode, len(plan.Tasks))
		}

		for i, w := range []struct{ id, file string }{{"a", x}, {"b", y}} {
			task := plan.Tasks[i]
			want := []string{h.exe, "--schema=" + schemas[w.id].Path, w.file, "--id", w.id, h.workspace, h.cache}

			if !slices.Equal(task.Args, want) || !slices.Equal(task.Files, []string{w.file}) {
				t.Errorf("%s task %d: args %q files %q, want %q", mode, i, task.Args, task.Files, want)
			}

			if v, _ := lookupEnv(task.Env, "FILE"); v != w.file {
				t.Errorf("%s task %d: FILE=%q, want %q", mode, i, v, w.file)
			}

			wantStdin := ""
			if mode == ModeStdin {
				wantStdin = w.file
			}

			if task.StdinFile != wantStdin {
				t.Errorf("%s task %d: StdinFile %q, want %q", mode, i, task.StdinFile, wantStdin)
			}
		}
	}
}

func TestBuildPlanInterpolatesInOnePass(t *testing.T) {
	h := newHarness(t)
	schemas := h.schemas("a")
	f := h.file("f.json", "")

	spec := h.spec(ModePerFile, "${TRICKY}", "{file}", "{{schema}}", "$${HOME}", "pre-${PLAIN}-{schema-id}")
	spec.Env["OUT"] = "${TRICKY}"
	spec.Env["BOTH"] = "{schema-id}:${PLAIN}"
	opts := h.options(spec)
	opts.Lookup = lookupFrom(map[string]string{"TRICKY": "{schema} ${PLAIN} $${X} {file}", "PLAIN": "p"})

	plan, err := BuildPlan([]Input{{Path: f, SchemaID: "a"}}, schemas, opts)
	if err != nil {
		t.Fatal(err)
	}

	task := plan.Tasks[0]
	want := []string{h.exe, "{schema} ${PLAIN} $${X} {file}", f, "{schema}", "${HOME}", "pre-p-a"}

	if !slices.Equal(task.Args, want) {
		t.Errorf("args = %q, want %q", task.Args, want)
	}

	if v, _ := lookupEnv(task.Env, "OUT"); v != "{schema} ${PLAIN} $${X} {file}" {
		t.Errorf("OUT = %q", v)
	}

	if v, _ := lookupEnv(task.Env, "BOTH"); v != "a:p" {
		t.Errorf("BOTH = %q", v)
	}
}

func TestBuildPlanEnvironment(t *testing.T) {
	h := newHarness(t)
	schemas := h.schemas("a")
	f := h.file("f.json", "")

	spec := h.spec(ModeBatch, "{files...}")
	spec.Env = map[string]string{"KEEP": "overlay", "ZNEW": "z", "ANEW": "{schema-id}"}
	opts := h.options(spec)
	opts.Environ = []string{"KEEP=base", "OTHER=1", "OTHER=2", "garbage", "", fakeVar + "=" + h.config}

	plan, err := BuildPlan([]Input{{Path: f, SchemaID: "a"}}, schemas, opts)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"KEEP=overlay", "OTHER=2", fakeVar + "=" + h.config, "ANEW=a", "ZNEW=z"}
	if got := plan.Tasks[0].Env; !slices.Equal(got, want) {
		t.Errorf("inherited env = %q, want %q", got, want)
	}

	opts.Spec.InheritEnv = false

	plan, err = BuildPlan([]Input{{Path: f, SchemaID: "a"}}, schemas, opts)
	if err != nil {
		t.Fatal(err)
	}

	want = []string{"ANEW=a", "KEEP=overlay", "ZNEW=z"}
	if got := plan.Tasks[0].Env; !slices.Equal(got, want) {
		t.Errorf("isolated env = %q, want %q", got, want)
	}

	opts.Spec.Env = nil

	plan, err = BuildPlan([]Input{{Path: f, SchemaID: "a"}}, schemas, opts)
	if err != nil {
		t.Fatal(err)
	}

	if got := plan.Tasks[0].Env; got == nil || len(got) != 0 {
		t.Errorf("empty env = %#v, want a non-nil empty slice", got)
	}
}

func TestBuildPlanRelativeCwd(t *testing.T) {
	h := newHarness(t)
	sub := filepath.Join(h.root, "base", "sub")

	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	spec := h.spec(ModeBatch, "{files...}")
	spec.Cwd = "sub"
	spec.CwdBase = filepath.Join(h.root, "base")

	plan, err := BuildPlan([]Input{{Path: h.file("f.json", ""), SchemaID: "a"}}, h.schemas("a"), h.options(spec))
	if err != nil {
		t.Fatal(err)
	}

	if plan.Tasks[0].Dir != sub {
		t.Errorf("Dir = %q, want %q", plan.Tasks[0].Dir, sub)
	}

	spec.Cwd = ""
	spec.CwdBase = ""

	plan, err = BuildPlan([]Input{{Path: h.file("f.json", ""), SchemaID: "a"}}, h.schemas("a"), h.options(spec))
	if err != nil {
		t.Fatal(err)
	}

	if plan.Tasks[0].Dir != h.workspace {
		t.Errorf("default Dir = %q, want the workspace %q", plan.Tasks[0].Dir, h.workspace)
	}
}

func TestBuildPlanEmptyInputs(t *testing.T) {
	h := newHarness(t)

	plan, err := BuildPlan(nil, nil, h.options(h.spec(ModeBatch, "{files...}")))
	if err != nil {
		t.Fatal(err)
	}

	if plan.Tasks == nil || len(plan.Tasks) != 0 {
		t.Errorf("tasks = %#v, want an empty list", plan.Tasks)
	}
}

func TestBuildPlanErrors(t *testing.T) {
	h := newHarness(t)
	f := h.file("f.json", "")
	notDir := h.file("plain.txt", "")

	cases := []struct {
		mutate func(*Options, *[]Input)
		name   string
		kind   fault.Kind
	}{
		{name: "invalid mode", kind: fault.Usage, mutate: func(o *Options, _ *[]Input) { o.Spec.Mode = "parallel" }},
		{name: "batch without files list", kind: fault.Usage, mutate: func(o *Options, _ *[]Input) { o.Spec.Args = []string{"{schema}"} }},
		{name: "unset variable in args", kind: fault.Usage, mutate: func(o *Options, _ *[]Input) { o.Spec.Args = []string{"${NOPE}", "{files...}"} }},
		{name: "unset variable in env", kind: fault.Usage, mutate: func(o *Options, _ *[]Input) { o.Spec.Env["X"] = "${NOPE}" }},
		{name: "unset variable in command", kind: fault.Usage, mutate: func(o *Options, _ *[]Input) { o.Spec.Command = "${NOPE}" }},
		{name: "empty command", kind: fault.Usage, mutate: func(o *Options, _ *[]Input) {
			o.Spec.Command = "${EMPTY}"
			o.Lookup = lookupFrom(map[string]string{"EMPTY": ""})
		}},
		{name: "empty cwd", kind: fault.Usage, mutate: func(o *Options, _ *[]Input) {
			o.Spec.Cwd = "${EMPTY}"
			o.Spec.CwdBase = h.root
			o.Lookup = lookupFrom(map[string]string{"EMPTY": ""})
		}},
		{name: "invalid env name", kind: fault.Usage, mutate: func(o *Options, _ *[]Input) { o.Spec.Env["A=B"] = "x" }},
		{name: "empty env name", kind: fault.Usage, mutate: func(o *Options, _ *[]Input) { o.Spec.Env[""] = "x" }},
		{name: "NUL in argument", kind: fault.Usage, mutate: func(o *Options, _ *[]Input) { o.Spec.Args = []string{"a\x00b", "{files...}"} }},
		{name: "NUL in input", kind: fault.Usage, mutate: func(_ *Options, in *[]Input) { (*in)[0].Path += "\x00" }},
		{name: "missing cwd", kind: fault.ConsumerStart, mutate: func(o *Options, _ *[]Input) { o.Spec.Cwd = filepath.Join(h.root, "missing") }},
		{name: "cwd is a file", kind: fault.ConsumerStart, mutate: func(o *Options, _ *[]Input) { o.Spec.Cwd = notDir }},
		{name: "relative cwd without base", kind: fault.Internal, mutate: func(o *Options, _ *[]Input) { o.Spec.Cwd = "sub" }},
		{name: "missing executable", kind: fault.ConsumerStart, mutate: func(o *Options, _ *[]Input) { o.Spec.Command = filepath.Join(h.root, "missing") }},
		{name: "executable is a directory", kind: fault.ConsumerStart, mutate: func(o *Options, _ *[]Input) { o.Spec.Command = h.workspace }},
		{name: "bare name without PATH", kind: fault.ConsumerStart, mutate: func(o *Options, _ *[]Input) { o.Spec.Command = "definitely-not-installed-consumer" }},
		{name: "relative input", kind: fault.Internal, mutate: func(_ *Options, in *[]Input) { (*in)[0].Path = "f.json" }},
		{name: "unknown schema", kind: fault.Internal, mutate: func(_ *Options, in *[]Input) { (*in)[0].SchemaID = "zzz" }},
		{name: "relative workspace", kind: fault.Internal, mutate: func(o *Options, _ *[]Input) { o.Workspace = "ws" }},
		{name: "relative cache", kind: fault.Internal, mutate: func(o *Options, _ *[]Input) { o.CacheDir = "cache" }},
		{name: "no cache for {cache} in args", kind: fault.Internal, mutate: func(o *Options, _ *[]Input) {
			o.CacheDir = ""
			o.Spec.Args = []string{"--cache={cache}", "{files...}"}
		}},
		{name: "no cache for {cache} in env", kind: fault.Internal, mutate: func(o *Options, _ *[]Input) {
			o.CacheDir = ""
			o.Spec.Env["CACHE"] = "{cache}/x"
		}},
		{name: "no cache for {cache} in command", kind: fault.Internal, mutate: func(o *Options, _ *[]Input) {
			o.CacheDir = ""
			o.Spec.Command = "{cache}/bin/consumer"
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := h.options(h.spec(ModeBatch, "{files...}"))
			inputs := []Input{{Path: f, SchemaID: "a"}}
			c.mutate(&opts, &inputs)

			_, err := BuildPlan(inputs, h.schemas("a"), opts)
			wantKind(t, err, c.kind)
		})
	}
}

// A local schema is an ordinary schema to the runner: it is grouped by ID
// with the catalog schemas, its own file is {schema} and "local:<path>" is
// {schema-ref}, and its origin reaches the report.
func TestLocalSchemasGroupAndExpandLikeCatalogSchemas(t *testing.T) {
	h := newHarness(t)
	schemas := h.schemas("a")
	schemas["company"] = SchemaInfo{
		ID:     "company",
		Path:   h.file("schemas/company.schema.json", "{}"),
		Ref:    "local:schemas/company.schema.json",
		Origin: "local",
	}

	c1, a1, c2 := h.file("config/one.json", "{}"), h.file("a1.json", "{}"), h.file("config/two.json", "{}")
	inputs := []Input{{Path: c1, SchemaID: "company"}, {Path: a1, SchemaID: "a"}, {Path: c2, SchemaID: "company"}}

	spec := h.spec(ModeBatch, "{schema}", "--id={schema-id}", "--ref={schema-ref}", "{files...}")
	spec.Env["SCHEMA_FILE"] = "{schema}"
	opts := h.options(spec)

	plan, err := BuildPlan(inputs, schemas, opts)
	if err != nil {
		t.Fatal(err)
	}

	want := []struct {
		id, origin string
		files      []string
	}{{"company", "local", []string{c1, c2}}, {"a", "catalog", []string{a1}}}

	if len(plan.Tasks) != len(want) {
		t.Fatalf("plan has %d tasks, want %d: %+v", len(plan.Tasks), len(want), plan.Tasks)
	}

	for i, w := range want {
		task, s := plan.Tasks[i], schemas[w.id]
		wantArgs := slices.Concat([]string{h.exe, s.Path, "--id=" + w.id, "--ref=" + s.Ref}, w.files)

		if task.SchemaID != w.id || task.SchemaRef != s.Ref || task.SchemaOrigin != w.origin || !slices.Equal(task.Args, wantArgs) {
			t.Errorf("task %d = %+v, want schema %s (%s) and args %q", i, task, w.id, w.origin, wantArgs)
		}

		if v, _ := lookupEnv(task.Env, "SCHEMA_FILE"); v != s.Path {
			t.Errorf("task %d: SCHEMA_FILE = %q, want %q", i, v, s.Path)
		}
	}

	report, err := Execute(t.Context(), plan, opts)
	if err != nil {
		t.Fatal(err)
	}

	for i, w := range want {
		if got := report.Tasks[i]; got.SchemaID != w.id || got.Origin != w.origin || got.SchemaRef != schemas[w.id].Ref || got.Status != StatusOK {
			t.Errorf("report task %d = %+v, want %s from %s", i, got, w.id, w.origin)
		}
	}

	records := h.records()
	if len(records) != 2 || records[0].Key != "company" || !slices.Contains(records[0].Args, schemas["company"].Path) {
		t.Errorf("consumers = %+v, want the local schema's own file first", records)
	}
}

// A run on local schemas alone may have no cache directory at all; only a
// template that uses {cache} needs one.
func TestBuildPlanWithoutCacheDir(t *testing.T) {
	h := newHarness(t)
	f := h.file("f.json", "{}")

	opts := h.options(h.spec(ModeBatch, "{schema}", "{files...}"))
	opts.CacheDir = ""

	plan, err := BuildPlan([]Input{{Path: f, SchemaID: "a"}}, h.schemas("a"), opts)
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{h.exe, h.schemas("a")["a"].Path, f}; len(plan.Tasks) != 1 || !slices.Equal(plan.Tasks[0].Args, want) {
		t.Errorf("tasks = %+v, want args %q", plan.Tasks, want)
	}
}

func TestSpecUses(t *testing.T) {
	spec := Spec{Command: "c", Cwd: DefaultCwd, Args: []string{"{schema}", "{files...}"}, Env: map[string]string{"A": "${HOME}", "B": "{{cache}}"}}
	if spec.Uses(interp.Cache) || !spec.Uses(interp.Workspace) || !spec.Uses(interp.Schema) || !spec.Uses(interp.Files) {
		t.Errorf("Uses of %+v is wrong", spec)
	}

	for _, s := range []Spec{
		{Command: "{cache}/c"},
		{Command: "c", Cwd: "{cache}"},
		{Command: "c", Args: []string{"--dir={cache}"}},
		{Command: "c", Env: map[string]string{"X": "{cache}"}},
	} {
		if !s.Uses(interp.Cache) {
			t.Errorf("%+v uses {cache}", s)
		}
	}

	if (Spec{Command: "{cache"}).Uses(interp.Cache) {
		t.Error("a template that does not parse uses nothing")
	}
}
