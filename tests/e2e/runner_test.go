//go:build e2e

package e2e

import (
	"bytes"
	"encoding/base64"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestE23_BatchGrouping runs interleaved inputs of two schemas in batch mode:
// one consumer per schema ID, groups in order of first appearance, files in
// input order (duplicates dropped), each group with its own {schema}.
func TestE23_BatchGrouping(t *testing.T) {
	t.Parallel()

	set := runnerPublish(t)

	cases := []struct {
		name   string
		inputs []string
		want   []runnerGroup
	}{
		{
			name: "alpha first",
			inputs: []string{
				"alpha.json", "config/app.toml", ".github/workflows/ci.yml", "nested/beta.yaml",
				"nested/deep/alpha.json", "config/sub/db.toml", "./alpha.json",
			},
			want: []runnerGroup{
				{ID: "alpha", Files: []string{"alpha.json", ".github/workflows/ci.yml", "nested/deep/alpha.json"}},
				{ID: "beta", Files: []string{"config/app.toml", "nested/beta.yaml", "config/sub/db.toml"}},
			},
		},
		{
			name:   "beta first",
			inputs: []string{"nested/beta.yaml", "alpha.json", "config/app.toml", "nested/deep/alpha.json"},
			want: []runnerGroup{
				{ID: "beta", Files: []string{"nested/beta.yaml", "config/app.toml"}},
				{ID: "alpha", Files: []string{"alpha.json", "nested/deep/alpha.json"}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ws := sandboxOf(t).Workspace
			runnerWriteInputs(t, ws, tc.inputs...)

			logDir := t.TempDir()
			bin := binPath("testconsumer")
			cfg := set.config(t, runnerConfig{
				Mode:    "batch",
				Command: bin,
				Args:    []string{"--schemafile", "{schema}", "--schema-id={schema-id}", "{files...}", "--after-files"},
			})
			reportPath := filepath.Join(t.TempDir(), "report.json")

			res := runnerRun(t, runOpts{
				Env:   []string{"TC_LOG_DIR=" + logDir, "TC_READ_STDIN=1"},
				Stdin: []byte("schepherd's own stdin must not reach a batch consumer\n"),
			}, cfg, []string{"--report", reportPath}, tc.inputs...).ok(t)
			if len(res.Stdout) != 0 {
				t.Fatalf("run wrote to stdout although the consumer printed nothing:\n%s", res)
			}

			recs := runnerRecords(t, logDir)
			if len(recs) != len(tc.want) {
				t.Fatalf("%d consumer processes, want %d (one per schema ID): %+v", len(recs), len(tc.want), recs)
			}

			schemaPaths := map[string]bool{}

			for i, g := range tc.want {
				schema := runnerSchemaPath(t, cfg, g.ID)
				schemaPaths[schema] = true

				want := slices.Concat([]string{bin, "--schemafile", schema, "--schema-id=" + g.ID}, runnerAbs(ws, g.Files...), []string{"--after-files"})
				if !slices.Equal(recs[i].Argv, want) {
					t.Errorf("process %d argv\n got %q\nwant %q", i, recs[i].Argv, want)
				}

				runnerCheckSchemaFile(t, recs[i], schema, preparedSchema(t, set.prepared, g.ID))

				if !recs[i].StdinIsDevNull {
					t.Errorf("process %d: stdin is not the null device in batch mode", i)
				}

				if got := runnerStdin(t, recs[i]); len(got) != 0 {
					t.Errorf("process %d read %q from stdin in batch mode, want nothing", i, got)
				}
			}

			if len(schemaPaths) != len(tc.want) {
				t.Errorf("groups share a {schema} path: %v", slices.Collect(maps.Keys(schemaPaths)))
			}

			report := decodeJSON[runnerReport](t, readFile(t, reportPath))
			if report.ReportVersion != 1 || report.Mode != "batch" || report.ExitCode != 0 || len(report.Tasks) != len(tc.want) {
				t.Fatalf("report %+v", report)
			}

			for i, g := range tc.want {
				task := report.Tasks[i]
				if task.SchemaID != g.ID || task.SchemaRef != set.ref(t, g.ID) || task.Status != "ok" || task.ExitCode != 0 ||
					!slices.Equal(task.Files, runnerAbs(ws, g.Files...)) {
					t.Errorf("report task %d = %+v, want schema %s (%s) with files %q, status ok",
						i, task, g.ID, set.ref(t, g.ID), runnerAbs(ws, g.Files...))
				}
			}
		})
	}
}

// TestE24_PerFile starts one consumer per input, including two inputs of the
// same schema, and hands every input over as an absolute path made from the
// current directory.
func TestE24_PerFile(t *testing.T) {
	t.Parallel()

	set := runnerPublish(t)
	ws := sandboxOf(t).Workspace
	contents := runnerWriteInputs(t, ws, "alpha.json", "config/app.toml", ".github/workflows/ci.yml", "nested/beta.yaml")

	bin := binPath("testconsumer")
	cfg := set.config(t, runnerConfig{
		Mode:    "per-file",
		Command: bin,
		Args:    []string{"--schemafile", "{schema}", "{file}"},
		Env:     map[string]string{"SCHEMA_ID": "{schema-id}", "SCHEMA_FILE": "{file}"},
	})

	cases := []struct {
		name      string
		dir       string
		flags     []string
		inputs    []string
		wantIDs   []string
		wantFiles []string
	}{
		{
			name:      "from the workspace",
			dir:       ws,
			inputs:    []string{"alpha.json", "./config/app.toml", filepath.Join(ws, ".github", "workflows", "ci.yml"), "nested/../nested/beta.yaml"},
			wantIDs:   []string{"alpha", "beta", "alpha", "beta"},
			wantFiles: []string{"alpha.json", "config/app.toml", ".github/workflows/ci.yml", "nested/beta.yaml"},
		},
		{
			name:      "from a subdirectory",
			dir:       filepath.Join(ws, "nested"),
			flags:     []string{"--workspace", ws},
			inputs:    []string{"beta.yaml", "../alpha.json"},
			wantIDs:   []string{"beta", "alpha"},
			wantFiles: []string{"nested/beta.yaml", "alpha.json"},
		},
	}

	for _, tc := range cases {
		logDir := t.TempDir()
		args := slices.Concat(tc.flags, []string{"--config", cfg, "run", "--"}, tc.inputs)
		cli(t, runOpts{
			Dir:   tc.dir,
			Env:   []string{"TC_LOG_DIR=" + logDir, "TC_READ_STDIN=1"},
			Stdin: []byte("schepherd's own stdin must not reach a per-file consumer\n"),
		}, args...).ok(t)

		recs := runnerRecords(t, logDir)
		if len(recs) != len(tc.inputs) {
			t.Fatalf("%s: %d consumer processes for %d inputs, want one per input: %+v", tc.name, len(recs), len(tc.inputs), recs)
		}

		pids := map[int]bool{}

		for i, rec := range recs {
			id, rel := tc.wantIDs[i], tc.wantFiles[i]
			abs := filepath.Join(ws, filepath.FromSlash(rel))
			schema := runnerSchemaPath(t, cfg, id)

			if want := []string{bin, "--schemafile", schema, abs}; !slices.Equal(rec.Argv, want) {
				t.Errorf("%s: process %d argv\n got %q\nwant %q", tc.name, i, rec.Argv, want)
			}

			if len(rec.Argv) == 4 && !filepath.IsAbs(rec.Argv[3]) {
				t.Errorf("%s: process %d got a relative input path %q", tc.name, i, rec.Argv[3])
			}

			if rec.Env["SCHEMA_FILE"] != abs || rec.Env["SCHEMA_ID"] != id {
				t.Errorf("%s: process %d env SCHEMA_FILE=%q SCHEMA_ID=%q, want %q and %q", tc.name, i, rec.Env["SCHEMA_FILE"], rec.Env["SCHEMA_ID"], abs, id)
			}

			if got, want := rec.ArgFiles[abs], runnerFileOf([]byte(contents[rel])); got != want {
				t.Errorf("%s: process %d read %s as %+v, want %+v", tc.name, i, abs, got, want)
			}

			runnerCheckSchemaFile(t, rec, schema, preparedSchema(t, set.prepared, id))

			if !rec.StdinIsDevNull {
				t.Errorf("%s: process %d: stdin is not the null device in per-file mode", tc.name, i)
			}

			if got := runnerStdin(t, rec); len(got) != 0 {
				t.Errorf("%s: process %d read %q from stdin in per-file mode, want nothing", tc.name, i, got)
			}

			pids[rec.PID] = true
		}

		if len(pids) != len(recs) {
			t.Errorf("%s: inputs shared a process: %+v", tc.name, recs)
		}
	}
}

// TestE25_Stdin streams every byte-exact stdin fixture (CRLF, comments,
// Unicode, no final newline) to its own consumer process; the input never
// appears in argv and inputs are never concatenated, also with jobs > 1.
func TestE25_Stdin(t *testing.T) {
	t.Parallel()

	set := runnerPublish(t)
	ws := sandboxOf(t).Workspace
	writeStdinFixtures(t, filepath.Join(ws, "stdin"))

	// Neither ascending nor descending, so a product that sorts its inputs
	// cannot pass the jobs=1 order check.
	names := []string{"unicode.json", "comments.yaml", "no-trailing-newline.json", "crlf.json", "mixed.yaml"}
	if !slices.Equal(slices.Sorted(slices.Values(names)), slices.Sorted(maps.Keys(stdinFixtures))) {
		t.Fatalf("input order %q does not cover the stdin fixtures %q", names, slices.Sorted(maps.Keys(stdinFixtures)))
	}

	if slices.IsSorted(names) || slices.IsSortedFunc(names, func(a, b string) int { return strings.Compare(b, a) }) {
		t.Fatalf("input order %q is sorted", names)
	}

	inputs := make([]string, 0, len(names))

	for _, name := range names {
		inputs = append(inputs, "stdin/"+name)
	}

	bin := binPath("testconsumer")

	for _, jobs := range []int{1, 3} {
		logDir := t.TempDir()
		cfg := set.config(t, runnerConfig{
			Mode:    "stdin",
			Command: bin,
			Args:    []string{"validate", "{schema}", "-"},
			Jobs:    jobs,
			Env:     map[string]string{"SCHEMA_FILE": "{file}", "TC_READ_STDIN": "1"},
		})

		runnerRun(t, runOpts{Env: []string{"TC_LOG_DIR=" + logDir}}, cfg, []string{"--schema", "alpha"}, inputs...).ok(t)

		recs := runnerRecords(t, logDir)
		if len(recs) != len(inputs) {
			t.Fatalf("jobs=%d: %d consumer processes for %d inputs, want one per input", jobs, len(recs), len(inputs))
		}

		schema := runnerSchemaPath(t, cfg, "alpha")
		byFile := map[string]runnerRecord{}

		for i, rec := range recs {
			if jobs == 1 {
				if want := filepath.Join(ws, "stdin", names[i]); rec.Env["SCHEMA_FILE"] != want {
					t.Errorf("jobs=1: process %d got {file} %q, want %q (input order)", i, rec.Env["SCHEMA_FILE"], want)
				}
			}

			if _, dup := byFile[rec.Env["SCHEMA_FILE"]]; dup {
				t.Errorf("jobs=%d: two processes for %s", jobs, rec.Env["SCHEMA_FILE"])
			}

			byFile[rec.Env["SCHEMA_FILE"]] = rec

			if want := []string{bin, "validate", schema, "-"}; !slices.Equal(rec.Argv, want) {
				t.Errorf("jobs=%d: argv %q, want %q", jobs, rec.Argv, want)
			}

			if rec.StdinIsDevNull {
				t.Errorf("jobs=%d: stdin of %s is the null device", jobs, rec.Env["SCHEMA_FILE"])
			}

			runnerCheckSchemaFile(t, rec, schema, preparedSchema(t, set.prepared, "alpha"))

			if len(rec.ArgFiles) != 1 {
				t.Errorf("jobs=%d: argv names files other than the schema: %v", jobs, rec.ArgFiles)
			}
		}

		for _, name := range names {
			file := filepath.Join(ws, "stdin", name)

			rec, ok := byFile[file]
			if !ok {
				t.Errorf("jobs=%d: no process received %s", jobs, name)

				continue
			}

			if got := runnerStdin(t, rec); !bytes.Equal(got, stdinFixtures[name]) {
				t.Errorf("jobs=%d: stdin of %s\n got %q\nwant %q", jobs, name, got, stdinFixtures[name])
			}
		}
	}
}

// TestE26_EnvCwd checks the consumer's command, environment and working
// directory: the command from ${TC_BIN}, expanded runner.env values over the
// inherited environment, cwd = {workspace}/sub, and a bare command name
// looked up only in the consumer's own (overridden) PATH.
func TestE26_EnvCwd(t *testing.T) {
	t.Parallel()

	set := runnerPublish(t)
	consumer := binPath("testconsumer")

	t.Run("command from the environment, runner.env and cwd", func(t *testing.T) {
		t.Parallel()

		sb := sandboxOf(t)
		ws := sb.Workspace
		runnerWriteInputs(t, ws, "alpha.json")
		runnerMkdir(t, filepath.Join(ws, "sub"))

		logDir := t.TempDir()
		cfg := set.config(t, runnerConfig{
			Mode:    "per-file",
			Command: "${TC_BIN}",
			Args:    []string{"{file}"},
			Cwd:     "{workspace}/sub",
			Env: map[string]string{
				"SCHEMA_ID":        "{schema-id}",
				"SCHEMA_REF":       "{schema-ref}",
				"SCHEMA_PATH":      "{schema}",
				"SCHEMA_WORKSPACE": "{workspace}",
				"SCHEMA_CACHE":     "{cache}",
				"SCHEMA_FROM_ENV":  "value-${TC_VALUE}-{schema-id}",
				"TC_OVERRIDE":      "inner",
				"NO_COLOR":         "1",
			},
		})

		runnerRun(t, runOpts{Env: []string{
			"TC_LOG_DIR=" + logDir, "TC_BIN=" + consumer, "TC_VALUE=outer-value",
			"TC_OVERRIDE=outer", "TC_INHERITED=yes",
		}}, cfg, nil, "alpha.json").ok(t)

		rec := runnerOnlyRecord(t, logDir)
		schema := runnerSchemaPath(t, cfg, "alpha")

		if want := []string{consumer, filepath.Join(ws, "alpha.json")}; !slices.Equal(rec.Argv, want) {
			t.Errorf("argv %q, want %q", rec.Argv, want)
		}

		if want := filepath.Join(ws, "sub"); rec.Cwd != want {
			t.Errorf("consumer cwd %q, want %q", rec.Cwd, want)
		}

		wantEnv := map[string]string{
			"SCHEMA_ID":        "alpha",
			"SCHEMA_REF":       set.ref(t, "alpha"),
			"SCHEMA_PATH":      schema,
			"SCHEMA_WORKSPACE": ws,
			"SCHEMA_CACHE":     sb.CacheDir,
			"SCHEMA_FROM_ENV":  "value-outer-value-alpha",
			"TC_OVERRIDE":      "inner",
			"NO_COLOR":         "1",
			"TC_INHERITED":     "yes",
			"TC_BIN":           consumer,
			"TC_VALUE":         "outer-value",
			"TC_LOG_DIR":       logDir,
			"PATH":             runnerSandboxPath(),
		}
		if !maps.Equal(rec.Env, wantEnv) {
			t.Errorf("consumer env\n got %v\nwant %v", rec.Env, wantEnv)
		}

		if !strings.HasPrefix(schema, sb.CacheDir+string(filepath.Separator)) {
			t.Errorf("{schema} %s is not below {cache} %s", schema, sb.CacheDir)
		}
	})

	t.Run("inherit_env = false", func(t *testing.T) {
		t.Parallel()

		ws := sandboxOf(t).Workspace
		runnerWriteInputs(t, ws, "alpha.json")

		logDir := t.TempDir()
		cfg := set.config(t, runnerConfig{
			Mode:       "per-file",
			Command:    "${TC_BIN}",
			Args:       []string{"{file}"},
			InheritEnv: new(false),
			Env:        map[string]string{"TC_LOG_DIR": "${TC_OUTER_LOG_DIR}", "SCHEMA_ID": "{schema-id}"},
		})

		runnerRun(t, runOpts{Env: []string{
			"TC_OUTER_LOG_DIR=" + logDir, "TC_BIN=" + consumer, "TC_INHERITED=yes",
		}}, cfg, nil, "alpha.json").ok(t)

		rec := runnerOnlyRecord(t, logDir)
		if want := map[string]string{"TC_LOG_DIR": logDir, "SCHEMA_ID": "alpha"}; !maps.Equal(rec.Env, want) {
			t.Errorf("consumer env %v, want only runner.env %v", rec.Env, want)
		}

		if rec.Cwd != ws {
			t.Errorf("consumer cwd %q, want the workspace %q", rec.Cwd, ws)
		}

		// testconsumer records only some variables; env(1) prints every one,
		// so HOME, TMPDIR, XDG_*, DOCKER_CONFIG or TZ leaking in is visible.
		cases := []struct {
			name string
			spec runnerConfig
			want []string
		}{
			{
				name: "per-file with runner.env",
				spec: runnerConfig{
					Mode: "per-file", Command: runnerEnvBin, InheritEnv: new(false),
					Env: map[string]string{"SCHEMA_FILE": "{file}", "ONLY": "1"},
				},
				want: []string{"ONLY=1", "SCHEMA_FILE=" + filepath.Join(ws, "alpha.json")},
			},
			{
				name: "stdin without runner.env",
				spec: runnerConfig{Mode: "stdin", Command: runnerEnvBin, InheritEnv: new(false)},
				want: []string{},
			},
		}

		for _, tc := range cases {
			res := runnerRun(t, runOpts{Env: []string{"TC_INHERITED=yes"}}, set.config(t, tc.spec), nil, "alpha.json").ok(t)

			got := []string{}
			if len(res.Stdout) != 0 {
				got = strings.Split(strings.TrimSuffix(string(res.Stdout), "\n"), "\n")
			}

			slices.Sort(got)

			if !slices.Equal(got, tc.want) {
				t.Errorf("%s: consumer environment %q, want exactly %q\n%s", tc.name, got, tc.want, res)
			}
		}
	})

	t.Run("bare command in the overridden PATH", func(t *testing.T) {
		t.Parallel()

		ws := sandboxOf(t).Workspace
		runnerWriteInputs(t, ws, "alpha.json")

		privateBin := runnerPrivateConsumer(t, filepath.Join(t.TempDir(), "private-bin"))
		consumerPath := privateBin + string(os.PathListSeparator) + "/usr/bin:/bin"

		logDir := t.TempDir()
		cfg := set.config(t, runnerConfig{
			Mode:    "batch",
			Command: runnerPrivateName,
			Args:    []string{"{files...}"},
			Env:     map[string]string{"PATH": "${TC_PRIVATE_PATH}"},
		})

		runnerRun(t, runOpts{Env: []string{"TC_LOG_DIR=" + logDir, "TC_PRIVATE_PATH=" + consumerPath}}, cfg, nil, "alpha.json").ok(t)

		rec := runnerOnlyRecord(t, logDir)
		if want := []string{runnerPrivateName, filepath.Join(ws, "alpha.json")}; !slices.Equal(rec.Argv, want) {
			t.Errorf("argv %q, want %q", rec.Argv, want)
		}

		if rec.Env["PATH"] != consumerPath {
			t.Errorf("consumer PATH %q, want the overridden %q", rec.Env["PATH"], consumerPath)
		}
	})

	t.Run("bare command missing from the consumer PATH", func(t *testing.T) {
		t.Parallel()

		ws := sandboxOf(t).Workspace
		runnerWriteInputs(t, ws, "alpha.json")

		privateBin := runnerPrivateConsumer(t, filepath.Join(t.TempDir(), "private-bin"))
		runnerPrivateConsumer(t, ws)
		runnerPrivateConsumer(t, filepath.Join(ws, "rel-bin"))

		cases := []struct {
			name       string
			outerPath  string
			runnerPath string
		}{
			{name: "not overridden", outerPath: runnerSandboxPath()},
			{name: "only in the PATH of schepherd", outerPath: privateBin + ":" + runnerSandboxPath(), runnerPath: "/usr/bin:/bin"},
			{name: "only via relative and empty entries", outerPath: runnerSandboxPath(), runnerPath: ".::rel-bin:/usr/bin:/bin"},
		}

		for _, tc := range cases {
			logDir := t.TempDir()
			spec := runnerConfig{Mode: "batch", Command: runnerPrivateName, Args: []string{"{files...}"}}

			if tc.runnerPath != "" {
				spec.Env = map[string]string{"PATH": tc.runnerPath}
			}

			res := runnerRun(t, runOpts{Env: []string{"TC_LOG_DIR=" + logDir, "PATH=" + tc.outerPath}}, set.config(t, spec), nil, "alpha.json").wantCode(t, 7)
			if !strings.Contains(string(res.Stderr), runnerPrivateName) || len(res.Stdout) != 0 {
				t.Errorf("%s: stderr does not name the missing command (or stdout is not empty):\n%s", tc.name, res)
			}

			if recs := runnerRecords(t, logDir); len(recs) != 0 {
				t.Errorf("%s: a consumer ran: %+v", tc.name, recs)
			}
		}
	})

	t.Run("missing cwd", func(t *testing.T) {
		t.Parallel()

		ws := sandboxOf(t).Workspace
		runnerWriteInputs(t, ws, "alpha.json")

		logDir := t.TempDir()
		cfg := set.config(t, runnerConfig{Mode: "batch", Command: consumer, Args: []string{"{files...}"}, Cwd: "{workspace}/sub"})

		res := runnerRun(t, runOpts{Env: []string{"TC_LOG_DIR=" + logDir}}, cfg, nil, "alpha.json").wantCode(t, 7)
		if !strings.Contains(string(res.Stderr), filepath.Join(ws, "sub")) {
			t.Errorf("stderr does not name the missing working directory:\n%s", res)
		}

		if recs := runnerRecords(t, logDir); len(recs) != 0 {
			t.Errorf("a consumer ran: %+v", recs)
		}
	})
}

// TestE27_NoShell passes file names and argument templates full of shell
// syntax; the consumer, a direct child of schepherd, receives every one of
// them literally, and nothing is ever executed by a shell.
func TestE27_NoShell(t *testing.T) {
	t.Parallel()

	set := runnerPublish(t)

	names := []string{
		"with space.json",
		"two  spaces\tand tab.json",
		`single'quote.json`,
		`double"quote.json`,
		"$(touch pwned).json",
		"`touch pwned`.json",
		"semi;touch pwned;.json",
		"pipe|touch pwned.json",
		"amp && touch pwned &.json",
		"redirect >pwned.json",
		"-leading-dash.json",
		"--help",
		"*.json",
		"~tilde.json",
		"$HOME.json",
		"${HOME}.json",
		"{schema}.json",
		"back\\slash.json",
		"new\nline.json",
		"üñîçødé ☃ \U0001F411.json",
	}
	metaArgs := []string{
		"$(touch pwned)", "`touch pwned`", "; touch pwned", "| touch pwned", "&& touch pwned", "> pwned",
		"*", "~", "~/x", "$HOME", "'single quoted'", `"double quoted"`, "a b", "",
	}

	cases := []struct {
		mode     string
		args     []string
		wantArgv func(bin, schema string, files []string) [][]string
	}{
		{
			mode: "batch",
			args: slices.Concat(metaArgs, []string{"{schema}", "{files...}"}),
			wantArgv: func(bin, schema string, files []string) [][]string {
				return [][]string{slices.Concat([]string{bin}, metaArgs, []string{schema}, files)}
			},
		},
		{
			mode: "per-file",
			args: slices.Concat(metaArgs, []string{"{schema}", "--file={file}", "{file}"}),
			wantArgv: func(bin, schema string, files []string) [][]string {
				out := make([][]string, 0, len(files))
				for _, f := range files {
					out = append(out, slices.Concat([]string{bin}, metaArgs, []string{schema, "--file=" + f, f}))
				}

				return out
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()

			sb := sandboxOf(t)
			contents := runnerWriteInputs(t, sb.Workspace, names...)

			logDir := t.TempDir()
			bin := binPath("testconsumer")
			cfg := set.config(t, runnerConfig{Mode: tc.mode, Command: bin, Args: tc.args})

			p := startBin(t, "schepherd", runOpts{Env: []string{"TC_LOG_DIR=" + logDir}},
				slices.Concat([]string{"--config", cfg, "run", "--schema", "alpha", "--"}, names)...)
			if res := p.Wait(); res.Code != 0 {
				t.Fatalf("run failed:\n%s", res)
			}

			files := runnerAbs(sb.Workspace, names...)
			want := tc.wantArgv(bin, runnerSchemaPath(t, cfg, "alpha"), files)

			recs := runnerRecords(t, logDir)
			if len(recs) != len(want) {
				t.Fatalf("%d consumer processes, want %d", len(recs), len(want))
			}

			for i, rec := range recs {
				if !slices.Equal(rec.Argv, want[i]) {
					t.Errorf("process %d argv\n got %q\nwant %q", i, rec.Argv, want[i])
				}

				if rec.PPID != p.PID() {
					t.Errorf("process %d has parent %d, want schepherd itself (%d)", i, rec.PPID, p.PID())
				}
			}

			for i, name := range names {
				rec := recs[0]
				if tc.mode == "per-file" {
					rec = recs[i]
				}

				if got, want := rec.ArgFiles[files[i]], runnerFileOf([]byte(contents[name])); got != want {
					t.Errorf("consumer read %q as %+v, want %+v", files[i], got, want)
				}
			}

			for _, root := range []string{sb.Dir, filepath.Dir(cfg), logDir} {
				for _, entry := range fileTree(t, root) {
					if base := filepath.Base(strings.TrimSuffix(entry, "/")); base == "pwned" || strings.HasPrefix(base, "pwned") {
						t.Errorf("a shell ran: %s exists below %s", entry, root)
					}
				}
			}
		})
	}
}

// TestE28_Interpolation checks the two template namespaces: configuration
// errors (unset variable, unknown placeholder, misplaced {files...}, ...)
// exit 2 before any consumer starts, a substituted value is never scanned
// again, and $${X}, {{ and }} are literal escapes.
func TestE28_Interpolation(t *testing.T) {
	t.Parallel()

	set := runnerPublish(t)
	consumer := binPath("testconsumer")

	t.Run("configuration errors", func(t *testing.T) {
		t.Parallel()

		batch := func(args ...string) runnerConfig {
			return runnerConfig{Mode: "batch", Command: consumer, Args: args}
		}
		withEnv := func(c runnerConfig, key, value string) runnerConfig {
			c.Env = map[string]string{key: value}

			return c
		}

		// Every case breaks exactly one rule, and stderr must name the field
		// or the rule it broke, so an unrelated usage error cannot pass.
		cases := []struct {
			name   string
			spec   runnerConfig
			stderr []string
		}{
			{
				name:   "unset variable in command",
				spec:   runnerConfig{Mode: "batch", Command: "${TC_E28_UNSET}", Args: []string{"{files...}"}},
				stderr: []string{"runner.command", "TC_E28_UNSET"},
			},
			{name: "unset variable in args", spec: batch("${TC_E28_UNSET}", "{files...}"), stderr: []string{"runner.args[0]", "TC_E28_UNSET"}},
			{
				name:   "unset variable in env",
				spec:   withEnv(batch("{files...}"), "SCHEMA_X", "${TC_E28_UNSET}"),
				stderr: []string{"runner.env.SCHEMA_X", "TC_E28_UNSET"},
			},
			{
				name:   "unset variable in cwd",
				spec:   runnerConfig{Mode: "batch", Command: consumer, Args: []string{"{files...}"}, Cwd: "${TC_E28_UNSET}"},
				stderr: []string{"runner.cwd", "TC_E28_UNSET"},
			},
			{name: "unknown placeholder in args", spec: batch("{nope}", "{files...}"), stderr: []string{"runner.args[0]", "{nope}"}},
			{
				name:   "unknown placeholder in env",
				spec:   withEnv(batch("{files...}"), "SCHEMA_X", "{schema_id}"),
				stderr: []string{"runner.env.SCHEMA_X", "{schema_id}"},
			},
			{
				name:   "embedded files list",
				spec:   batch("--files={files...}", "{files...}"),
				stderr: []string{"runner.args[0]", "{files...}", "whole"},
			},
			{
				name:   "files list in env",
				spec:   withEnv(batch("{files...}"), "SCHEMA_X", "{files...}"),
				stderr: []string{"runner.env.SCHEMA_X", "{files...}"},
			},
			{name: "two files lists", spec: batch("{files...}", "{files...}"), stderr: []string{"runner.args", "exactly one", "{files...}"}},
			{name: "batch without files list", spec: batch("{schema}"), stderr: []string{"runner.args", "exactly one", "{files...}"}},
			{name: "file in batch mode", spec: batch("{file}", "{files...}"), stderr: []string{"{file}", "batch"}},
			{
				name:   "files list in per-file mode",
				spec:   runnerConfig{Mode: "per-file", Command: consumer, Args: []string{"{file}", "{files...}"}},
				stderr: []string{"runner.args[1]", "{files...}", "batch"},
			},
			{
				name:   "per-file without file",
				spec:   runnerConfig{Mode: "per-file", Command: consumer, Args: []string{"{schema}"}},
				stderr: []string{"per-file", "{file}"},
			},
			{name: "lone opening brace", spec: batch("{", "{files...}"), stderr: []string{"runner.args[0]", `"{"`}},
			{name: "lone closing brace", spec: batch("}", "{files...}"), stderr: []string{"runner.args[0]", `"}"`}},
			{name: "unterminated placeholder", spec: batch("{schema", "{files...}"), stderr: []string{"runner.args[0]", `"{schema"`}},
			{
				name:   "runtime placeholder in command",
				spec:   runnerConfig{Mode: "batch", Command: "{schema}", Args: []string{"{files...}"}},
				stderr: []string{"runner.command", "{schema}"},
			},
			{
				name:   "runtime placeholder in cwd",
				spec:   runnerConfig{Mode: "batch", Command: consumer, Args: []string{"{files...}"}, Cwd: "{schema}"},
				stderr: []string{"runner.cwd", "{schema}"},
			},
		}

		ws := sandboxOf(t).Workspace
		runnerWriteInputs(t, ws, "alpha.json")

		for _, tc := range cases {
			logDir := t.TempDir()

			res := runnerRun(t, runOpts{Env: []string{"TC_LOG_DIR=" + logDir}}, set.config(t, tc.spec), nil, "alpha.json")
			if res.Code != 2 {
				t.Errorf("%s: want exit code 2, got:\n%s", tc.name, res)

				continue
			}

			if len(tc.stderr) == 0 {
				t.Fatalf("%s: case names no expected stderr text", tc.name)
			}

			explained := true
			for _, part := range tc.stderr {
				explained = explained && strings.Contains(string(res.Stderr), part)
			}

			if len(res.Stdout) != 0 || !explained {
				t.Errorf("%s: stdout must be empty and stderr must explain the error (mentioning all of %q):\n%s", tc.name, tc.stderr, res)
			}

			if recs := runnerRecords(t, logDir); len(recs) != 0 {
				t.Errorf("%s: a consumer ran: %+v", tc.name, recs)
			}
		}
	})

	t.Run("literals and escapes", func(t *testing.T) {
		t.Parallel()

		ws := sandboxOf(t).Workspace
		runnerWriteInputs(t, ws, "alpha.json")

		const tricky = "{schema} {file} {files...} {schema-id} ${HOME} $${TC_BIN} {{x}} }{ {"

		logDir := t.TempDir()
		cfg := set.config(t, runnerConfig{
			Mode:    "batch",
			Command: "${TC_BIN}",
			Args: []string{
				"${TC_TRICKY}", "$${TC_TRICKY}", "{{schema}}", "{{{schema-id}}}", "}}{{", "pre-{schema-id}-post",
				"$HOME", "$${NOT_SET_ANYWHERE}", "${TC_TRICKY}|{schema-id}", "{files...}",
			},
			Env: map[string]string{
				"SCHEMA_TRICKY":  "${TC_TRICKY}",
				"SCHEMA_ESCAPED": "$${HOME}:{{schema}}:{{{schema-id}}}",
				"SCHEMA_MIXED":   "${TC_TRICKY}|{schema-id}",
			},
		})

		runnerRun(t, runOpts{Env: []string{"TC_LOG_DIR=" + logDir, "TC_BIN=" + consumer, "TC_TRICKY=" + tricky}}, cfg, nil, "alpha.json").ok(t)

		rec := runnerOnlyRecord(t, logDir)

		want := []string{
			consumer, tricky, "${TC_TRICKY}", "{schema}", "{alpha}", "}{", "pre-alpha-post",
			"$HOME", "${NOT_SET_ANYWHERE}", tricky + "|alpha", filepath.Join(ws, "alpha.json"),
		}
		if !slices.Equal(rec.Argv, want) {
			t.Errorf("argv\n got %q\nwant %q", rec.Argv, want)
		}

		wantEnv := map[string]string{
			"SCHEMA_TRICKY":  tricky,
			"SCHEMA_ESCAPED": "${HOME}:{schema}:{alpha}",
			"SCHEMA_MIXED":   tricky + "|alpha",
		}
		for key, value := range wantEnv {
			if rec.Env[key] != value {
				t.Errorf("env %s = %q, want %q", key, rec.Env[key], value)
			}
		}
	})
}

// runnerPrivateName is a command name that exists in no directory of the
// sandbox PATH, only where runnerPrivateConsumer puts it.
const runnerPrivateName = "e2e-private-consumer"

// runnerEnvBin prints its whole environment, one KEY=VALUE per line.
const runnerEnvBin = "/usr/bin/env"

// runnerSet is set-basic published by the real publisher into the calling
// test's own repository.
type runnerSet struct {
	repo     string
	catalog  string
	prepared string
	doc      catalogDoc
}

// runnerConfig is the [runner] section of a scenario's configuration.
type runnerConfig struct {
	InheritEnv *bool
	Env        map[string]string
	Mode       string
	Command    string
	Cwd        string
	Args       []string
	Jobs       int
}

type runnerGroup struct {
	ID    string
	Files []string
}

// runnerRecord is one invocation record of the testconsumer.
type runnerRecord struct {
	Env            map[string]string        `json:"env"`
	ArgFiles       map[string]runnerArgFile `json:"argFiles"`
	Stdin          *string                  `json:"stdin"`
	Name           string                   `json:"-"`
	Cwd            string                   `json:"cwd"`
	Argv           []string                 `json:"argv"`
	PID            int                      `json:"pid"`
	PPID           int                      `json:"ppid"`
	StdinIsDevNull bool                     `json:"stdinIsDevNull"`
}

type runnerArgFile struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// runnerReport is the --report document of docs/runner.md.
type runnerReport struct {
	Mode  string `json:"mode"`
	Tasks []struct {
		SchemaID   string   `json:"schemaId"`
		SchemaRef  string   `json:"schemaRef"`
		Status     string   `json:"status"`
		Files      []string `json:"files"`
		ExitCode   int      `json:"exitCode"`
		DurationMs int64    `json:"durationMs"`
	} `json:"tasks"`
	Skipped []struct {
		File   string `json:"file"`
		Reason string `json:"reason"`
	} `json:"skipped"`
	ReportVersion int `json:"reportVersion"`
	ExitCode      int `json:"exitCode"`
}

// runnerPublish publishes set-basic into repoPath(t) of the source registry
// and reads the published catalog back directly from the registry.
func runnerPublish(t *testing.T) runnerSet {
	t.Helper()

	path := repoPath(t)
	repo := suite.source.Repo(path)
	pub := publishSet(t, repo, newSet(t, "set-basic"), "--now", "20260101.0000")

	doc := suite.source.Catalog(t, path, pub.CatalogDigest).Doc

	return runnerSet{repo: repo, catalog: pub.CatalogDigest, prepared: pub.Prepared, doc: doc}
}

// config writes a client configuration for the set with the given runner
// section into a fresh directory and returns its path.
func (s runnerSet) config(t *testing.T, r runnerConfig) string {
	t.Helper()

	return writeConfig(t, t.TempDir(), clientConfig{Repository: s.repo, Catalog: s.catalog, Extra: r.toml()}.TOML())
}

// ref is the documented {schema-ref} of a schema: repository@manifest digest.
func (s runnerSet) ref(t *testing.T, id string) string {
	t.Helper()

	return s.repo + "@" + s.doc.entry(t, id).Artifact.Digest
}

func (r runnerConfig) toml() string {
	var b strings.Builder

	b.WriteString("[runner]\n")

	if r.Mode != "" {
		b.WriteString("mode = " + tomlString(r.Mode) + "\n")
	}

	b.WriteString("command = " + tomlString(r.Command) + "\n")

	quoted := make([]string, 0, len(r.Args))
	for _, a := range r.Args {
		quoted = append(quoted, tomlString(a))
	}

	b.WriteString("args = [" + strings.Join(quoted, ", ") + "]\n")

	if r.Cwd != "" {
		b.WriteString("cwd = " + tomlString(r.Cwd) + "\n")
	}

	if r.Jobs != 0 {
		b.WriteString("jobs = " + strconv.Itoa(r.Jobs) + "\n")
	}

	if r.InheritEnv != nil {
		b.WriteString("inherit_env = " + strconv.FormatBool(*r.InheritEnv) + "\n")
	}

	if len(r.Env) > 0 {
		b.WriteString("\n[runner.env]\n")

		for _, key := range slices.Sorted(maps.Keys(r.Env)) {
			b.WriteString(tomlString(key) + " = " + tomlString(r.Env[key]) + "\n")
		}
	}

	return b.String()
}

// runnerRun runs "schepherd --config cfg run <flags> -- <files>".
func runnerRun(t *testing.T, o runOpts, cfg string, flags []string, files ...string) result {
	t.Helper()

	return cli(t, o, slices.Concat([]string{"--config", cfg, "run"}, flags, []string{"--"}, files)...)
}

// runnerSchemaPath returns what "schepherd path <id>" prints for the test's
// sandbox, the documented value of {schema}.
func runnerSchemaPath(t *testing.T, cfg, id string) string {
	t.Helper()

	res := cli(t, runOpts{}, "--config", cfg, "path", id).ok(t)

	path, ok := strings.CutSuffix(string(res.Stdout), "\n")
	if !ok || !filepath.IsAbs(path) {
		t.Fatalf("path %s printed %q", id, res.Stdout)
	}

	return path
}

// runnerWriteInputs writes one small, distinct document per relative path
// below ws and returns the contents by path.
func runnerWriteInputs(t *testing.T, ws string, rels ...string) map[string]string {
	t.Helper()

	out := make(map[string]string, len(rels))

	for _, rel := range rels {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
		if _, done := out[clean]; done {
			continue
		}

		content := `{"name":` + tomlString(clean) + `,"port":1}` + "\n"
		writeFile(t, filepath.Join(ws, filepath.FromSlash(clean)), []byte(content))
		out[clean] = content
		out[rel] = content
	}

	return out
}

// runnerAbs joins slash-separated relative paths to ws.
func runnerAbs(ws string, rels ...string) []string {
	out := make([]string, 0, len(rels))
	for _, rel := range rels {
		out = append(out, filepath.Join(ws, filepath.FromSlash(rel)))
	}

	return out
}

func runnerMkdir(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// runnerPrivateConsumer copies the testconsumer into dir as
// runnerPrivateName and returns dir.
func runnerPrivateConsumer(t *testing.T, dir string) string {
	t.Helper()

	runnerMkdir(t, dir)

	if err := os.WriteFile(filepath.Join(dir, runnerPrivateName), readFile(t, binPath("testconsumer")), 0o755); err != nil {
		t.Fatal(err)
	}

	return dir
}

// runnerSandboxPath is the PATH of every binary run by cli (see
// sandbox.environ).
func runnerSandboxPath() string {
	return suite.bin + string(os.PathListSeparator) + "/usr/local/bin:/usr/bin:/bin"
}

// runnerRecords returns the testconsumer records in dir in the order in
// which they were written.
func runnerRecords(t *testing.T, dir string) []runnerRecord {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	type stamped struct {
		rec  runnerRecord
		nano int64
	}

	var all []stamped

	for _, e := range entries {
		stem, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || strings.HasPrefix(stem, ".") {
			continue
		}

		prefix, _, _ := strings.Cut(stem, "-")

		nano, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatalf("record name %q: %v", e.Name(), err)
		}

		rec := decodeJSON[runnerRecord](t, readFile(t, filepath.Join(dir, e.Name())))
		rec.Name = e.Name()
		all = append(all, stamped{rec: rec, nano: nano})
	}

	slices.SortStableFunc(all, func(a, b stamped) int {
		switch {
		case a.nano < b.nano:
			return -1
		case a.nano > b.nano:
			return 1
		default:
			return strings.Compare(a.rec.Name, b.rec.Name)
		}
	})

	out := make([]runnerRecord, 0, len(all))
	for _, s := range all {
		out = append(out, s.rec)
	}

	return out
}

func runnerOnlyRecord(t *testing.T, dir string) runnerRecord {
	t.Helper()

	recs := runnerRecords(t, dir)
	if len(recs) != 1 {
		t.Fatalf("%d consumer processes, want 1: %+v", len(recs), recs)
	}

	return recs[0]
}

// runnerCheckSchemaFile asserts that the consumer could read the {schema}
// path it received and that it holds exactly the prepared schema bytes.
func runnerCheckSchemaFile(t *testing.T, rec runnerRecord, schema string, prepared []byte) {
	t.Helper()

	if got, want := rec.ArgFiles[schema], runnerFileOf(prepared); got != want {
		t.Errorf("consumer read {schema} %s as %+v, want %+v", schema, got, want)
	}
}

func runnerFileOf(data []byte) runnerArgFile {
	return runnerArgFile{SHA256: sha256Hex(data), Size: int64(len(data))}
}

func runnerStdin(t *testing.T, rec runnerRecord) []byte {
	t.Helper()

	if rec.Stdin == nil {
		t.Fatalf("consumer did not record its stdin: %+v", rec)
	}

	data, err := base64.StdEncoding.DecodeString(*rec.Stdin)
	if err != nil {
		t.Fatal(err)
	}

	return data
}
