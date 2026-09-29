//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Paths inside the validators container (E31, E32).
const (
	consumersCheckJSONSchemaBin = "/usr/local/bin/check-jsonschema"
	consumersSourcemetaBin      = "/usr/local/bin/jsonschema"
	consumersContainerBin       = "/e2e/bin/schepherd"
	consumersContainerCache     = "/e2e/cache"
	consumersContainerWorkspace = "/e2e/ws"
	consumersContainerConfigs   = "/e2e/config"
	consumersContainerReport    = "/e2e/report"
)

// Runner sections of the real validators, as a user would write them
// (examples/check-jsonschema.schepherd.toml, examples/schepherd.toml). The
// verbose variant only adds check-jsonschema's -vv, which lists every file
// a successful invocation checked.
const (
	consumersCheckJSONSchemaRunner = `
[runner]
mode = "batch"
command = "${CHECK_JSONSCHEMA_BIN}"
args = ["--schemafile", "{schema}", "{files...}"]
`
	consumersCheckJSONSchemaVerboseRunner = `
[runner]
mode = "batch"
command = "${CHECK_JSONSCHEMA_BIN}"
args = ["-vv", "--schemafile", "{schema}", "{files...}"]
`
	consumersSourcemetaRunner = `
[runner]
mode = "batch"
command = "${JSONSCHEMA_BIN}"
args = ["validate", "{schema}", "{files...}"]
`
)

// consumersRecord is one invocation record of the testconsumer.
type consumersRecord struct {
	Env        map[string]string `json:"env"`
	Role       string            `json:"role"`
	Cwd        string            `json:"cwd"`
	Argv       []string          `json:"argv"`
	PID        int               `json:"pid"`
	PPID       int               `json:"ppid"`
	Grandchild int               `json:"grandchild"`
}

// consumersReport is the part of a run report (docs/runner.md) these
// scenarios check.
type consumersReport struct {
	Mode  string `json:"mode"`
	Tasks []struct {
		SchemaID  string   `json:"schemaId"`
		SchemaRef string   `json:"schemaRef"`
		Status    string   `json:"status"`
		Files     []string `json:"files"`
		ExitCode  int      `json:"exitCode"`
	} `json:"tasks"`
	ReportVersion int `json:"reportVersion"`
	ExitCode      int `json:"exitCode"`
}

// consumersTask is one expected task of a run report inside the validators
// container; files are relative to the container workspace.
type consumersTask struct {
	schemaID string
	status   string
	files    []string
	exitCode int
}

// consumersTaskFor is the single task of a run whose inputs, the arguments
// after "--", all belong to schema id and whose consumer exits with code.
func consumersTaskFor(id string, code int, args []string) []consumersTask {
	status := "ok"
	if code != 0 {
		status = "failed"
	}

	files := args[slices.Index(args, "--")+1:]

	return []consumersTask{{schemaID: id, status: status, files: files, exitCode: code}}
}

// consumersCatalog is the part of the catalog document (docs/oci-format.md)
// that names each schema's artifact.
type consumersCatalog struct {
	Schemas []struct {
		ID       string `json:"id"`
		Artifact struct {
			Digest string `json:"digest"`
		} `json:"artifact"`
	} `json:"schemas"`
}

// TestE29_Extends loads a child configuration whose base lives in another
// directory: the base's relative workspace and runner.cwd resolve against
// the base's directory, the child's runner.args replace the base's and
// runner.env tables merge key by key. Cycles and unknown keys anywhere in
// the chain are configuration errors (exit 2).
func TestE29_Extends(t *testing.T) {
	t.Parallel()

	repo, catalog := consumersPublish(t)

	root := consumersRealTempDir(t)
	shared := filepath.Join(root, "shared")
	project := filepath.Join(root, "project")
	childDir := filepath.Join(root, "child", "dir")
	elsewhere := filepath.Join(root, "elsewhere")

	// Every candidate directory has a tools/ subdirectory, so a cwd resolved
	// against the wrong file shows up as a wrong cwd rather than a start
	// failure.
	for _, dir := range []string{shared, project, childDir, elsewhere} {
		if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	settings := filepath.Join(project, "config", "app", "settings.toml")
	alpha := filepath.Join(project, "alpha.json")
	writeFile(t, settings, readFile(t, instance("beta", "valid.toml")))
	writeFile(t, alpha, readFile(t, instance("alpha", "valid.json")))

	base := consumersWithTopLevel(clientConfig{Repository: repo, Catalog: catalog, Extra: `
[runner]
mode = "batch"
command = ` + tomlString(binPath("testconsumer")) + `
args = ["--base-only", "{schema}", "{files...}"]
cwd = "tools"
timeout = "2m"

[runner.env]
SCHEMA_E29_BASE = "base"
SCHEMA_E29_SHARED = "from-base"
SCHEMA_E29_WORKSPACE = "{workspace}"
`}.TOML(), `workspace = "../project"`)
	writeFile(t, filepath.Join(shared, "base.toml"), []byte(base))

	child := filepath.Join(childDir, "child.toml")
	writeFile(t, child, []byte(`config_version = 1
extends = ["../../shared/base.toml"]

[runner]
args = ["--child", "{schema-id}", "{files...}"]

[runner.env]
SCHEMA_E29_CHILD = "child"
SCHEMA_E29_SHARED = "from-child"
`))

	t.Run("config check accepts the chain", func(t *testing.T) {
		t.Parallel()

		res := cli(t, runOpts{Dir: elsewhere}, "--config", child, "config", "check").ok(t)
		if len(res.Stdout) == 0 {
			t.Fatalf("config check printed nothing:\n%s", res)
		}
	})

	t.Run("run", func(t *testing.T) {
		t.Parallel()

		logDir := t.TempDir()
		cli(t, runOpts{Dir: elsewhere, Env: []string{"TC_LOG_DIR=" + logDir}}, "--config", child, "run", "--",
			filepath.Join("..", "project", "config", "app", "settings.toml"), filepath.Join("..", "project", "alpha.json")).ok(t)

		recs := consumersRecords(t, logDir)
		if len(recs) != 2 {
			t.Fatalf("want 2 consumer processes (beta, then alpha), got %d: %+v", len(recs), recs)
		}

		want := [][]string{
			{"--child", "beta", settings},
			{"--child", "alpha", alpha},
		}

		for i, rec := range recs {
			if !slices.Equal(rec.Argv[1:], want[i]) {
				t.Errorf("process %d: argv %q, want %q (the child's args replace the base's)", i, rec.Argv[1:], want[i])
			}

			if cwd := consumersRealPath(t, rec.Cwd); cwd != filepath.Join(shared, "tools") {
				t.Errorf("process %d: cwd %s, want %s (relative to the base file)", i, cwd, filepath.Join(shared, "tools"))
			}

			wantEnv := map[string]string{
				"SCHEMA_E29_BASE":      "base",
				"SCHEMA_E29_CHILD":     "child",
				"SCHEMA_E29_SHARED":    "from-child",
				"SCHEMA_E29_WORKSPACE": project,
			}
			for k, v := range wantEnv {
				if got, ok := rec.Env[k]; !ok || got != v {
					t.Errorf("process %d: %s=%q (set %v), want %q", i, k, got, ok, v)
				}
			}
		}
	})

	t.Run("cycle", func(t *testing.T) {
		t.Parallel()

		dir := consumersRealTempDir(t)
		a := filepath.Join(dir, "shared", "a.toml")
		b := filepath.Join(dir, "child", "dir", "b.toml")
		self := filepath.Join(dir, "self.toml")

		writeFile(t, a, []byte("config_version = 1\nextends = [\"../child/dir/b.toml\"]\n"))
		writeFile(t, b, []byte("config_version = 1\nextends = [\"../../shared/a.toml\"]\n"))
		writeFile(t, self, []byte("config_version = 1\nextends = [\"./self.toml\"]\n"))

		for _, file := range []string{a, b, self} {
			res := cli(t, runOpts{}, "--config", file, "config", "check").wantCode(t, 2)
			if len(res.Stdout) != 0 || !strings.Contains(string(res.Stderr), "cycle") {
				t.Errorf("config check of %s: want an extends cycle error on stderr only:\n%s", file, res)
			}
		}

		logDir := t.TempDir()
		cycling := filepath.Join(dir, "cycling.toml")
		writeFile(t, cycling, []byte("config_version = 1\nextends = [\"./cycling-base.toml\"]\n"))
		writeFile(t, filepath.Join(dir, "cycling-base.toml"), []byte(consumersWithTopLevel(clientConfig{
			Repository: repo, Catalog: catalog, Extra: consumersTestconsumerRunner(""),
		}.TOML(), `extends = ["./cycling.toml"]`)))

		cli(t, runOpts{Env: []string{"TC_LOG_DIR=" + logDir}}, "--config", cycling, "run", "--schema", "alpha", "--", alpha).wantCode(t, 2)

		if recs := consumersRecords(t, logDir); len(recs) != 0 {
			t.Fatalf("a consumer started although the configuration has a cycle: %+v", recs)
		}
	})

	t.Run("unknown keys", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name, base, child, key, file string
		}{
			{name: "top level of the base", base: "config_version = 1\nshell = \"/bin/sh\"\n", key: "shell", file: "base.toml"},
			{name: "runner table of the base", base: "config_version = 1\n\n[runner]\nshell = true\n", key: "shell", file: "base.toml"},
			{name: "catalog table of the child", base: "config_version = 1\n", child: "\n[catalog]\ntag = \"catalog-latest\"\n", key: "tag", file: "child.toml"},
		}

		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()

				dir := consumersRealTempDir(t)
				writeFile(t, filepath.Join(dir, "shared", "base.toml"), []byte(c.base))

				childFile := filepath.Join(dir, "child", "dir", "child.toml")
				writeFile(t, childFile, []byte("config_version = 1\nextends = [\"../../shared/base.toml\"]\n"+c.child))

				res := cli(t, runOpts{}, "--config", childFile, "config", "check").wantCode(t, 2)

				stderr := string(res.Stderr)
				if len(res.Stdout) != 0 || !strings.Contains(stderr, c.key) || !strings.Contains(stderr, c.file) {
					t.Fatalf("want an error naming the unknown key %q and %s on stderr only:\n%s", c.key, c.file, res)
				}
			})
		}
	})
}

// TestE30_ExitCancel checks the run's exit status for every documented
// consumer outcome: a consumer's own status passes through, a consumer that
// cannot start yields 7, a timeout yields 124 and an interrupted run 130,
// and in both of the latter cases the consumer's whole process group,
// grandchild included, is gone afterwards.
func TestE30_ExitCancel(t *testing.T) {
	t.Parallel()

	repo, catalog := consumersPublish(t)
	sb := sandboxOf(t)

	input := filepath.Join(sb.Workspace, "alpha.json")
	writeFile(t, input, readFile(t, instance("alpha", "valid.json")))

	config := func(t *testing.T, runner string) string {
		t.Helper()

		return writeConfig(t, t.TempDir(), clientConfig{Repository: repo, Catalog: catalog, Extra: runner}.TOML())
	}

	// A warm cache keeps the timing of the scenarios below independent of
	// the registry.
	cli(t, runOpts{Sandbox: sb}, "--config", config(t, consumersTestconsumerRunner("")), "path", "alpha").ok(t)

	run := func(t *testing.T, cfg string, env ...string) (res result, logDir, report string) {
		t.Helper()

		logDir, report = t.TempDir(), filepath.Join(t.TempDir(), "report.json")
		res = cli(t, runOpts{Sandbox: sb, Env: append([]string{"TC_LOG_DIR=" + logDir}, env...), Timeout: 2 * time.Minute},
			"--config", cfg, "run", "--report", report, "--", input)

		return res, logDir, report
	}

	start := func(t *testing.T, cfg string, env ...string) (p *process, logDir, report string) {
		t.Helper()

		logDir, report = t.TempDir(), filepath.Join(t.TempDir(), "report.json")
		p = startBin(t, "schepherd", runOpts{Sandbox: sb, Env: append([]string{"TC_LOG_DIR=" + logDir}, env...), Timeout: 2 * time.Minute},
			"--config", cfg, "run", "--report", report, "--", input)

		return p, logDir, report
	}

	t.Run("consumer exit status passes through", func(t *testing.T) {
		t.Parallel()

		res, logDir, report := run(t, config(t, consumersTestconsumerRunner("")), "TC_EXIT=3")
		res.wantCode(t, 3)

		if recs := consumersRecords(t, logDir); len(recs) != 1 {
			t.Fatalf("want exactly one consumer process, got %d: %+v", len(recs), recs)
		}

		consumersWantReport(t, report, "failed", 3)
	})

	t.Run("consumer cannot start", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		notExecutable := filepath.Join(dir, "not-executable-validator")
		writeFile(t, notExecutable, []byte("#!/bin/sh\nexit 0\n"))

		cases := []struct {
			name, command, extra, mention string
		}{
			{name: "missing absolute path", command: filepath.Join(dir, "no-such-validator"), mention: "no-such-validator"},
			{name: "bare name not on PATH", command: "schepherd-e2e-no-such-validator", mention: "schepherd-e2e-no-such-validator"},
			{name: "not executable", command: notExecutable, mention: "not-executable-validator"},
			{name: "missing cwd", command: binPath("testconsumer"), extra: `cwd = "{workspace}/no-such-cwd"`, mention: "no-such-cwd"},
		}

		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()

				res, logDir, report := run(t, config(t, consumersRunner(c.command, c.extra)))
				res.wantCode(t, 7)

				if len(res.Stdout) != 0 || !strings.Contains(string(res.Stderr), c.mention) {
					t.Fatalf("want an error naming %q on stderr only:\n%s", c.mention, res)
				}

				if recs := consumersRecords(t, logDir); len(recs) != 0 {
					t.Fatalf("a consumer ran: %+v", recs)
				}

				// docs/runner.md: "--report <file> writes a JSON report", and
				// start-error is the task status of a consumer that cannot be
				// started (exit status 7).
				if _, err := os.Stat(report); err != nil {
					t.Fatalf("run --report %s exited 7 without writing the report: %v", report, err)
				}

				consumersWantReport(t, report, "start-error", 7)
			})
		}
	})

	t.Run("timeout kills the process group", func(t *testing.T) {
		t.Parallel()

		res, logDir, report := run(t, config(t, consumersTestconsumerRunner(`timeout = "1s"`)), "TC_HANG=1", "TC_SPAWN_GRANDCHILD=1")
		res.wantCode(t, 124)

		recs := consumersRecords(t, logDir)
		parent := consumersParent(t, recs)

		// The grandchild writes its record right before it hangs, so it was
		// alive when the timeout hit and "gone" below is not vacuous.
		if !consumersHasGrandchild(recs, parent) {
			t.Fatalf("grandchild %d of consumer %d never reached its hang: %+v", parent.Grandchild, parent.PID, recs)
		}

		waitGone(t, parent.PID, 10*time.Second)
		waitGone(t, parent.Grandchild, 10*time.Second)
		consumersNoSurvivors(t, logDir)
		consumersWantReport(t, report, "timeout", 124)
	})

	for _, sig := range []struct {
		name   string
		signal syscall.Signal
	}{{"SIGINT", syscall.SIGINT}, {"SIGTERM", syscall.SIGTERM}} {
		t.Run(sig.name+" while the consumer hangs", func(t *testing.T) {
			t.Parallel()

			p, logDir, report := start(t, config(t, consumersTestconsumerRunner("")), "TC_HANG=1", "TC_SPAWN_GRANDCHILD=1")

			var parent consumersRecord

			eventually(t, time.Minute, "the consumer and its grandchild", func() bool {
				recs := consumersRecords(t, logDir)
				for _, rec := range recs {
					if rec.Role == "" && rec.Grandchild != 0 {
						parent = rec
					}
				}

				return parent.PID != 0 && consumersHasGrandchild(recs, parent)
			})

			if parent.PPID != p.PID() {
				t.Errorf("consumer %d has parent %d, want schepherd %d", parent.PID, parent.PPID, p.PID())
			}

			// The survivor check below must see the processes it looks for.
			if living := consumersLiving(t, logDir); !slices.Contains(living, parent.PID) || !slices.Contains(living, parent.Grandchild) {
				t.Fatalf("the process scan misses the running consumer %d or grandchild %d: %v", parent.PID, parent.Grandchild, living)
			}

			if err := p.Signal(sig.signal); err != nil {
				t.Fatal(err)
			}

			p.Wait().wantCode(t, 130)
			waitGone(t, parent.PID, 10*time.Second)
			waitGone(t, parent.Grandchild, 10*time.Second)
			consumersNoSurvivors(t, logDir)
			consumersWantReport(t, report, "canceled", 130)
		})
	}

	t.Run("consumer killed by a signal", func(t *testing.T) {
		t.Parallel()

		p, logDir, report := start(t, config(t, consumersTestconsumerRunner("")), "TC_HANG=1")

		var rec consumersRecord

		eventually(t, time.Minute, "the consumer", func() bool {
			recs := consumersRecords(t, logDir)
			if len(recs) == 1 {
				rec = recs[0]
			}

			return rec.PID != 0
		})

		if err := syscall.Kill(rec.PID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}

		p.Wait().wantCode(t, 128+int(syscall.SIGKILL))
		consumersWantReport(t, report, "failed", 128+int(syscall.SIGKILL))
	})
}

// TestE31_RealValidator runs "schepherd --offline run" with check-jsonschema
// inside the validators container without any network, on a cache warmed
// on the host: valid JSON, YAML and TOML instances pass, and every invalid
// one fails with check-jsonschema's own exit status 1. Every case runs with
// the documented configuration and again with -vv; the run report proves
// that every schema group ran against the pinned artifact, and -vv that
// check-jsonschema checked every file of each successful group.
func TestE31_RealValidator(t *testing.T) {
	t.Parallel()

	v := consumersValidatorsSetup(t)
	documented := v.config(t, "check-jsonschema.toml", consumersCheckJSONSchemaRunner)
	verbose := v.config(t, "check-jsonschema-vv.toml", consumersCheckJSONSchemaVerboseRunner)

	type validatorCase struct {
		name  string
		args  []string
		tasks []consumersTask
		want  int
	}

	single := func(name, id string, want int, args ...string) validatorCase {
		return validatorCase{name: name, args: args, want: want, tasks: consumersTaskFor(id, want, args)}
	}

	cases := []validatorCase{
		single("alpha valid JSON YAML TOML", "alpha", 0,
			"--schema", "alpha", "--", "instances/alpha/valid.json", "instances/alpha/valid.yaml", "instances/alpha/valid.toml"),
		single("beta valid JSON YAML TOML", "beta", 0,
			"--schema", "beta", "--", "instances/beta/valid.json", "instances/beta/valid.yaml", "instances/beta/valid.toml"),
		{name: "valid files resolved by fileMatch", want: 0, args: []string{"--", "alpha.json", "config/app/settings.toml"}, tasks: []consumersTask{
			{schemaID: "alpha", status: "ok", files: []string{"alpha.json"}},
			{schemaID: "beta", status: "ok", files: []string{"config/app/settings.toml"}},
		}},
		// fail_fast is off by default, so the beta group still runs after
		// the alpha group failed.
		{name: "invalid file resolved by fileMatch", want: 1, args: []string{"--", "alpha.json", "config/app/settings.toml", "broken/alpha.json"}, tasks: []consumersTask{
			{schemaID: "alpha", status: "failed", files: []string{"alpha.json", "broken/alpha.json"}, exitCode: 1},
			{schemaID: "beta", status: "ok", files: []string{"config/app/settings.toml"}},
		}},
		single("invalid TOML resolved by fileMatch", "beta", 1, "--", "config/broken/settings.toml"),
		single("alpha invalid JSON", "alpha", 1, "--schema", "alpha", "--", "instances/alpha/invalid.json"),
		single("alpha invalid YAML", "alpha", 1, "--schema", "alpha", "--", "instances/alpha/invalid.yaml"),
		single("alpha invalid TOML", "alpha", 1, "--schema", "alpha", "--", "instances/alpha/invalid.toml"),
		single("beta invalid JSON", "beta", 1, "--schema", "beta", "--", "instances/beta/invalid.json"),
		single("beta invalid YAML", "beta", 1, "--schema", "beta", "--", "instances/beta/invalid.yaml"),
		single("beta invalid TOML", "beta", 1, "--schema", "beta", "--", "instances/beta/invalid.toml"),
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			var invalid string
			if c.want != 0 {
				invalid = v.abs(c.args[len(c.args)-1])
			}

			for _, config := range []string{documented, verbose} {
				res, report := v.run(t, config, c.args...)
				res.wantCode(t, c.want)
				v.wantTasks(t, report, c.want, c.tasks)
				consumersWantCheckJSONSchema(t, res, c.tasks, invalid)

				if config != verbose {
					continue
				}

				var want []string

				for _, task := range c.tasks {
					if task.status == "ok" {
						want = append(want, v.absAll(task.files)...)
					}
				}

				if checked := consumersCheckedFiles(string(res.Stdout)); !slices.Equal(checked, want) {
					t.Fatalf("check-jsonschema -vv checked %q, want every file of the successful groups %q:\n%s", checked, want, res)
				}
			}
		})
	}
}

// TestE32_BackendSwap swaps the consumer from check-jsonschema to the
// Sourcemeta jsonschema CLI by configuration only: the same catalog pin, the
// same cache and the same schepherd binary (its sha256 is unchanged, also
// inside the container) now yield Sourcemeta's statuses, 0 for valid and 2
// for invalid instances, and both backends validate against the same schema
// artifacts of the pinned catalog.
func TestE32_BackendSwap(t *testing.T) {
	t.Parallel()

	binary := sha256Hex(readFile(t, binPath("schepherd")))
	t.Cleanup(func() {
		if after := sha256Hex(readFile(t, binPath("schepherd"))); after != binary {
			t.Errorf("schepherd binary changed during the scenario: %s, then %s", binary, after)
		}
	})

	v := consumersValidatorsSetup(t)
	checkCfg := v.config(t, "check-jsonschema.toml", consumersCheckJSONSchemaRunner)
	sourcemetaCfg := v.config(t, "sourcemeta.toml", consumersSourcemetaRunner)

	sum := dockerRunValidators(t, []mount{binMount()}, "sha256sum", consumersContainerBin).ok(t)
	if fields := strings.Fields(string(sum.Stdout)); len(fields) == 0 || fields[0] != binary {
		t.Fatalf("the container runs another schepherd binary than the host (%s):\n%s", binary, sum)
	}

	for _, cfg := range []string{checkCfg, sourcemetaCfg} {
		if got := v.schepherd(t, cfg, "catalog", "--json").ok(t); !bytes.Equal(got.Stdout, v.catalogJSON) {
			t.Fatalf("%s offline must print the pinned catalog the host fetched from the registry:\n%s\nhost:\n%s", cfg, got, v.catalogJSON)
		}
	}

	for _, id := range []string{"alpha", "beta"} {
		viaCheck := v.schepherd(t, checkCfg, "path", id).ok(t)
		viaSourcemeta := v.schepherd(t, sourcemetaCfg, "path", id).ok(t)

		if !strings.HasPrefix(string(viaCheck.Stdout), consumersContainerCache+"/") || !bytes.Equal(viaCheck.Stdout, viaSourcemeta.Stdout) {
			t.Fatalf("both configurations must hand the validator the same materialized %s in the cache:\n%s\n%s", id, viaCheck, viaSourcemeta)
		}
	}

	type backendCase struct {
		name   string
		config string
		schema string
		output string
		args   []string
		want   int
	}

	cases := []backendCase{
		{name: "check-jsonschema valid alpha", config: checkCfg, schema: "alpha", want: 0, output: "ok -- validation done", args: []string{
			"--schema", "alpha", "--", "instances/alpha/valid.json", "instances/alpha/valid.yaml",
		}},
		{name: "check-jsonschema valid beta", config: checkCfg, schema: "beta", want: 0, output: "ok -- validation done", args: []string{
			"--schema", "beta", "--", "instances/beta/valid.json", "instances/beta/valid.yaml",
		}},
		{name: "check-jsonschema invalid", config: checkCfg, schema: "alpha", want: 1, output: "Schema validation errors were encountered", args: []string{
			"--schema", "alpha", "--", "instances/alpha/invalid.json",
		}},
		{name: "sourcemeta valid alpha", config: sourcemetaCfg, schema: "alpha", want: 0, output: "2 validated, 2 passed, 0 failed", args: []string{
			"--schema", "alpha", "--", "instances/alpha/valid.json", "instances/alpha/valid.yaml",
		}},
		{name: "sourcemeta valid beta", config: sourcemetaCfg, schema: "beta", want: 0, output: "2 validated, 2 passed, 0 failed", args: []string{
			"--schema", "beta", "--", "instances/beta/valid.json", "instances/beta/valid.yaml",
		}},
		{name: "sourcemeta valid file resolved by fileMatch", config: sourcemetaCfg, schema: "alpha", want: 0, output: "1 validated, 1 passed, 0 failed", args: []string{
			"--", "alpha.json",
		}},
		{name: "sourcemeta invalid file resolved by fileMatch", config: sourcemetaCfg, schema: "alpha", want: 2, output: "Schema validation failure", args: []string{
			"--", "broken/alpha.json",
		}},
		{name: "sourcemeta alpha invalid JSON", config: sourcemetaCfg, schema: "alpha", want: 2, output: "Schema validation failure", args: []string{
			"--schema", "alpha", "--", "instances/alpha/invalid.json",
		}},
		{name: "sourcemeta alpha invalid YAML", config: sourcemetaCfg, schema: "alpha", want: 2, output: "Schema validation failure", args: []string{
			"--schema", "alpha", "--", "instances/alpha/invalid.yaml",
		}},
		{name: "sourcemeta beta invalid JSON", config: sourcemetaCfg, schema: "beta", want: 2, output: "Schema validation failure", args: []string{
			"--schema", "beta", "--", "instances/beta/invalid.json",
		}},
		{name: "sourcemeta beta invalid YAML", config: sourcemetaCfg, schema: "beta", want: 2, output: "Schema validation failure", args: []string{
			"--schema", "beta", "--", "instances/beta/invalid.yaml",
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			res, report := v.run(t, c.config, c.args...)
			res.wantCode(t, c.want)
			v.wantTasks(t, report, c.want, consumersTaskFor(c.schema, c.want, c.args))

			if out := string(res.Stdout) + string(res.Stderr); !strings.Contains(out, c.output) {
				t.Fatalf("validator output lacks %q:\n%s", c.output, res)
			}
		})
	}
}

// consumersValidators is a published set-basic snapshot with alpha and beta
// materialized in a host cache, a workspace of instances and a directory of
// configurations, all mounted into the validators container. catalogJSON is
// "catalog --json" of the pin on the host, refs the schemaRef
// (repository@schema manifest digest) of every schema in it.
type consumersValidators struct {
	refs          map[string]string
	repo, catalog string
	cache         string
	workspace     string
	configs       string
	catalogJSON   []byte
}

func consumersValidatorsSetup(t *testing.T) *consumersValidators {
	t.Helper()

	repo, catalog := consumersPublish(t)
	sb := sandboxOf(t)
	hostCfg := writeConfig(t, t.TempDir(), clientConfig{Repository: repo, Catalog: catalog}.TOML())

	for _, id := range []string{"alpha", "beta"} {
		cli(t, runOpts{Sandbox: sb}, "--config", hostCfg, "path", id).ok(t)
	}

	catalogJSON := cli(t, runOpts{Sandbox: sb}, "--config", hostCfg, "catalog", "--json").ok(t).Stdout
	doc := decodeJSON[consumersCatalog](t, catalogJSON)

	refs := make(map[string]string, len(doc.Schemas))
	for _, s := range doc.Schemas {
		refs[s.ID] = repo + "@" + s.Artifact.Digest
	}

	for _, id := range []string{"alpha", "beta"} {
		if !strings.HasPrefix(refs[id], repo+"@sha256:") {
			t.Fatalf("the pinned catalog has no artifact digest for %s:\n%s", id, catalogJSON)
		}
	}

	ws := t.TempDir()
	copyTree(t, fixture("instances"), filepath.Join(ws, "instances"))

	for rel, src := range map[string]string{
		"alpha.json":                  instance("alpha", "valid.json"),
		"config/app/settings.toml":    instance("beta", "valid.toml"),
		"broken/alpha.json":           instance("alpha", "invalid.json"),
		"config/broken/settings.toml": instance("beta", "invalid.toml"),
	} {
		writeFile(t, filepath.Join(ws, filepath.FromSlash(rel)), readFile(t, src))
	}

	return &consumersValidators{
		refs: refs, repo: repo, catalog: catalog, cache: sb.CacheDir, workspace: ws, configs: t.TempDir(), catalogJSON: catalogJSON,
	}
}

// config writes a configuration with the snapshot's pin and runner, but no
// registries tables (nothing is contacted offline), and returns its path
// inside the container.
func (v *consumersValidators) config(t *testing.T, name, runner string) string {
	t.Helper()

	none := map[string]*registrySettings{}
	for host := range defaultRegistrySettings() {
		none[host] = nil
	}

	writeFile(t, filepath.Join(v.configs, name), []byte(clientConfig{
		Repository: v.repo, Catalog: v.catalog, Registries: none, Extra: runner,
	}.TOML()))

	return consumersContainerConfigs + "/" + name
}

// schepherd runs "schepherd --config <config> --offline --cache-dir <cache>
// <args...>" inside the validators container with no network, the
// workspace as the working directory, and the validators' paths in
// CHECK_JSONSCHEMA_BIN and JSONSCHEMA_BIN.
func (v *consumersValidators) schepherd(t *testing.T, config string, args ...string) result {
	t.Helper()

	return v.schepherdWith(t, nil, config, args...)
}

// run is schepherd with "run --report <file> <args...>" and returns the
// report as well; the report directory is the only other writable mount.
func (v *consumersValidators) run(t *testing.T, config string, args ...string) (res result, report []byte) {
	t.Helper()

	dir := t.TempDir()
	res = v.schepherdWith(t, []mount{{Host: dir, Container: consumersContainerReport, Writable: true}}, config,
		append([]string{"run", "--report", consumersContainerReport + "/report.json"}, args...)...)

	return res, readFile(t, filepath.Join(dir, "report.json"))
}

func (v *consumersValidators) schepherdWith(t *testing.T, extra []mount, config string, args ...string) result {
	t.Helper()

	full := append([]string{consumersContainerBin, "--config", config, "--offline", "--cache-dir", consumersContainerCache}, args...)

	return dockerRun(t, validatorsImage(t), dockerOpts{
		Mounts: append([]mount{
			binMount(),
			{Host: v.cache, Container: consumersContainerCache, Writable: true},
			{Host: v.workspace, Container: consumersContainerWorkspace},
			{Host: v.configs, Container: consumersContainerConfigs},
		}, extra...),
		Network: "none",
		Workdir: consumersContainerWorkspace,
		Env:     []string{"CHECK_JSONSCHEMA_BIN=" + consumersCheckJSONSchemaBin, "JSONSCHEMA_BIN=" + consumersSourcemetaBin},
	}, full...)
}

// abs is the absolute path inside the container of a workspace file.
func (v *consumersValidators) abs(rel string) string {
	return consumersContainerWorkspace + "/" + rel
}

func (v *consumersValidators) absAll(rels []string) []string {
	out := make([]string, 0, len(rels))
	for _, rel := range rels {
		out = append(out, v.abs(rel))
	}

	return out
}

// wantTasks checks a batch run report: its exit status and exactly the
// expected tasks in task order, each with the pinned catalog's artifact of
// its schema as schemaRef.
func (v *consumersValidators) wantTasks(t *testing.T, data []byte, code int, want []consumersTask) {
	t.Helper()

	rep := decodeJSON[consumersReport](t, data)
	if rep.ReportVersion != 1 || rep.Mode != "batch" || rep.ExitCode != code || len(rep.Tasks) != len(want) {
		t.Fatalf("report: want version 1, mode batch, exitCode %d and %d tasks:\n%s", code, len(want), data)
	}

	for i, w := range want {
		got := rep.Tasks[i]
		if got.SchemaID != w.schemaID || got.SchemaRef != v.refs[w.schemaID] || got.Status != w.status ||
			got.ExitCode != w.exitCode || !slices.Equal(got.Files, v.absAll(w.files)) {
			t.Fatalf("report task %d: want %s (%s), status %q, exitCode %d, files %q:\n%s",
				i, w.schemaID, v.refs[w.schemaID], w.status, w.exitCode, v.absAll(w.files), data)
		}
	}
}

// consumersWantCheckJSONSchema checks check-jsonschema's stdout: exactly one
// success line per successful task, exactly one error summary per failed
// task, and the errors name invalid (when set).
func consumersWantCheckJSONSchema(t *testing.T, res result, tasks []consumersTask, invalid string) {
	t.Helper()

	var ok, failed int

	for _, task := range tasks {
		if task.status == "ok" {
			ok++
		} else {
			failed++
		}
	}

	out := string(res.Stdout)
	if n := strings.Count(out, "ok -- validation done"); n != ok {
		t.Fatalf("check-jsonschema reported success %d times, want once per successful group (%d):\n%s", n, ok, res)
	}

	if n := strings.Count(out, "Schema validation errors were encountered"); n != failed {
		t.Fatalf("check-jsonschema reported errors %d times, want once per failed group (%d):\n%s", n, failed, res)
	}

	if invalid != "" && !strings.Contains(out, invalid) {
		t.Fatalf("check-jsonschema did not report %s as invalid:\n%s", invalid, res)
	}
}

// consumersCheckedFiles returns, in order, the files that check-jsonschema
// -vv lists under "The following files were checked:" in all of its
// invocations' output.
func consumersCheckedFiles(stdout string) []string {
	var files []string

	listing := false

	for line := range strings.SplitSeq(stdout, "\n") {
		switch {
		case line == "The following files were checked:":
			listing = true
		case listing && strings.HasPrefix(line, "  "):
			files = append(files, strings.TrimPrefix(line, "  "))
		default:
			listing = false
		}
	}

	return files
}

// consumersPublish publishes set-basic into the test's own repository on the
// source registry (bypassing the shared proxy) and returns the repository
// and the catalog digest.
func consumersPublish(t *testing.T) (repo, catalog string) {
	t.Helper()

	repo = suite.source.Repo(repoPath(t))

	return repo, publishSet(t, repo, newSet(t, "set-basic"), "--now", "20260101.0000").CatalogDigest
}

// consumersRunner is a batch [runner] section for command with extra lines
// appended to the table.
func consumersRunner(command, extra string) string {
	return "[runner]\nmode = \"batch\"\ncommand = " + tomlString(command) + "\nargs = [\"{schema}\", \"{files...}\"]\n" + extra + "\n"
}

func consumersTestconsumerRunner(extra string) string {
	return consumersRunner(binPath("testconsumer"), extra)
}

// consumersWithTopLevel inserts top-level keys right after config_version,
// before the first table of a rendered clientConfig.
func consumersWithTopLevel(config, keys string) string {
	head, rest, _ := strings.Cut(config, "\n")

	return head + "\n" + strings.TrimSpace(keys) + "\n" + rest
}

// consumersRealTempDir is t.TempDir() with symlinks resolved, so paths the
// product reports can be compared with plain string equality.
func consumersRealTempDir(t *testing.T) string {
	t.Helper()

	return consumersRealPath(t, t.TempDir())
}

func consumersRealPath(t *testing.T, path string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}

	return resolved
}

// consumersRecords returns the testconsumer records in dir in the order they
// were written.
func consumersRecords(t *testing.T, dir string) []consumersRecord {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read consumer records: %v", err)
	}

	var out []consumersRecord

	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		out = append(out, decodeJSON[consumersRecord](t, readFile(t, filepath.Join(dir, e.Name()))))
	}

	return out
}

// consumersParent returns the record of the consumer that started a
// grandchild.
func consumersParent(t *testing.T, recs []consumersRecord) consumersRecord {
	t.Helper()

	for _, rec := range recs {
		if rec.Role == "" && rec.Grandchild != 0 {
			return rec
		}
	}

	t.Fatalf("no consumer record with a grandchild: %+v", recs)

	return consumersRecord{}
}

// consumersHasGrandchild reports whether recs hold the record of parent's
// grandchild, which the grandchild writes right before it hangs.
func consumersHasGrandchild(recs []consumersRecord, parent consumersRecord) bool {
	return slices.ContainsFunc(recs, func(r consumersRecord) bool {
		return r.PID == parent.Grandchild && r.Role == "grandchild" && r.PPID == parent.PID
	})
}

// consumersLiving returns the live processes (not zombies) whose
// environment carries the TC_LOG_DIR of logDir: schepherd itself while it
// runs, the consumer, its grandchild and anything they started.
func consumersLiving(t *testing.T, logDir string) []int {
	t.Helper()

	marker := []byte("\x00TC_LOG_DIR=" + logDir + "\x00")

	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("list processes: %v", err)
	}

	var pids []int

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		environ, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
		if err != nil {
			continue
		}

		if bytes.Contains(append([]byte{0}, environ...), marker) && processAlive(pid) {
			pids = append(pids, pid)
		}
	}

	return pids
}

// consumersNoSurvivors fails the test when any process of logDir's
// consumers is still alive.
func consumersNoSurvivors(t *testing.T, logDir string) {
	t.Helper()

	living := consumersLiving(t, logDir)
	alive := make([]string, 0, len(living))

	for _, pid := range living {
		cmdline, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
		alive = append(alive, fmt.Sprintf("%d (%s)", pid, bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '})))
	}

	if len(alive) > 0 {
		t.Fatalf("consumer processes survived: %s", strings.Join(alive, ", "))
	}
}

// consumersWantReport checks a run report with the single task of the E30
// input.
func consumersWantReport(t *testing.T, path, status string, code int) {
	t.Helper()

	data := readFile(t, path)
	rep := decodeJSON[consumersReport](t, data)

	if rep.ReportVersion != 1 || rep.Mode != "batch" || rep.ExitCode != code || len(rep.Tasks) != 1 {
		t.Fatalf("report: want version 1, mode batch, exitCode %d and one task:\n%s", code, data)
	}

	if task := rep.Tasks[0]; task.SchemaID != "alpha" || task.Status != status || task.ExitCode != code || len(task.Files) != 1 {
		t.Fatalf("report task: want alpha, status %q, exitCode %d, one file:\n%s", status, code, data)
	}
}
