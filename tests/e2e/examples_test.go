//go:build e2e

package e2e

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Paths inside the validators container of TestE31_ShippedExamples.
const (
	examplesContainerDir    = "/e2e/examples"
	examplesContainerValid  = "/e2e/ws"
	examplesContainerBroken = "/e2e/broken"
)

// examplesSummary is the part of `config check --json` that shows how an
// example and its base merged.
type examplesSummary struct {
	Files      []string `json:"files"`
	Repository string   `json:"repository"`
	Catalog    string   `json:"catalog"`
	Mappings   []struct {
		Schema    string   `json:"schema"`
		FileMatch []string `json:"fileMatch"`
	} `json:"mappings"`
	Runner struct {
		Env        map[string]string `json:"env"`
		Mode       string            `json:"mode"`
		Command    string            `json:"command"`
		Cwd        string            `json:"cwd"`
		Timeout    string            `json:"timeout"`
		Args       []string          `json:"args"`
		Jobs       int               `json:"jobs"`
		FailFast   bool              `json:"failFast"`
		InheritEnv bool              `json:"inheritEnv"`
	} `json:"runner"`
}

// examplesFile is the part of an example configuration file the scenario
// derives its expectations from.
type examplesFile struct {
	Extends []string `toml:"extends"`
	Runner  struct {
		Env        map[string]string `toml:"env"`
		Mode       string            `toml:"mode"`
		Command    string            `toml:"command"`
		Cwd        string            `toml:"cwd"`
		Timeout    string            `toml:"timeout"`
		Args       []string          `toml:"args"`
		Jobs       int               `toml:"jobs"`
		FailFast   *bool             `toml:"fail_fast"`
		InheritEnv *bool             `toml:"inherit_env"`
	} `toml:"runner"`
	Mappings []struct {
		Schema    string   `toml:"schema"`
		FileMatch []string `toml:"file_match"`
	} `toml:"mappings"`
}

// TestE31_ShippedExamples runs the configuration files shipped in examples/
// unchanged, each with its extends of base.schepherd.toml, against a
// catalog published to the TLS registry with credentials (found through the
// standard Docker configuration and trusted through SSL_CERT_FILE, since the
// examples have no [registries] table). The Sourcemeta examples, batch with
// the [[mappings]] override and stdin, run the pinned Sourcemeta CLI on the
// host; the check-jsonschema example runs in the validators container,
// offline on the cache the host runs filled.
func TestE31_ShippedExamples(t *testing.T) {
	t.Parallel()

	examples := filepath.Join(suite.root, "examples")
	reg := suite.auth
	repo := reg.Repo(repoPath(t))

	set := newSet(t, "set-basic")
	writeFile(t, filepath.Join(set, "schemas", "company-config.json"), readFile(t, fixture("examples", "company-config.json")))
	examplesAppend(t, filepath.Join(set, "source.toml"), `
[[entries]]
id = "company-config"
name = "e2e-set-basic"
description = "Company configuration of the shipped examples, reachable through [[mappings]] only"
url = "https://schemas.example.com/e2e/company-config.json"
file = "schemas/company-config.json"
`)

	catalog := publishSet(t, repo, set, "--now", "20260101.0000").CatalogDigest

	// Every step runs in this sandbox: its Docker configuration holds the
	// credential, and the container step mounts its default cache.
	sb := sandboxOf(t)
	writeDockerConfig(t, sb.DockerConfig, map[string]credential{reg.Host(): reg.User()})

	valid := sb.Workspace
	writeFile(t, filepath.Join(valid, "alpha.json"), readFile(t, instance("alpha", "valid.json")))
	writeFile(t, filepath.Join(valid, ".github", "workflows", "ci.yml"), readFile(t, instance("alpha", "valid.yaml")))
	writeFile(t, filepath.Join(valid, "config", "company.json"), readFile(t, fixture("examples", "company-valid.json")))
	writeFile(t, filepath.Join(valid, "config", "app", "settings.toml"), readFile(t, instance("beta", "valid.toml")))

	broken := t.TempDir()
	writeFile(t, filepath.Join(broken, "alpha.json"), readFile(t, instance("alpha", "invalid.json")))
	writeFile(t, filepath.Join(broken, "config", "company.json"), readFile(t, fixture("examples", "company-invalid.json")))

	env := []string{
		"SCHEPHERD_REPOSITORY=" + repo,
		"SCHEPHERD_CATALOG=" + catalog,
		"SSL_CERT_FILE=" + suite.pki.CAFile,
		"JSONSCHEMA_BIN=" + suite.jsonschema,
		"CHECK_JSONSCHEMA_BIN=" + consumersCheckJSONSchemaBin,
	}

	run := func(t *testing.T, dir, example string, extra []string, args ...string) (result, consumersReport) {
		t.Helper()

		report := filepath.Join(t.TempDir(), "report.json")
		full := append([]string{"--config", filepath.Join(examples, example), "run", "--report", report, "--"}, args...)
		res := cli(t, runOpts{Sandbox: sb, Dir: dir, Env: append(slices.Clone(env), extra...)}, full...)

		return res, decodeJSON[consumersReport](t, readFile(t, report))
	}

	t.Run("config check shows each example merged with its base", func(t *testing.T) {
		t.Parallel()

		for _, name := range []string{"schepherd.toml", "stdin.schepherd.toml", "check-jsonschema.schepherd.toml"} {
			examplesCheckMerge(t, sb, examples, name, env, repo, catalog)
		}
	})

	t.Run("sourcemeta batch example with the mapping override", func(t *testing.T) {
		t.Parallel()

		res, rep := run(t, valid, "schepherd.toml", nil, "config/company.json", "alpha.json")
		res.ok(t)
		examplesWantTasks(t, rep, "batch", 0, []consumersTask{
			{schemaID: "company-config", status: "ok", files: []string{filepath.Join(valid, "config", "company.json")}},
			{schemaID: "alpha", status: "ok", files: []string{filepath.Join(valid, "alpha.json")}},
		})

		if n := strings.Count(string(res.Stdout)+string(res.Stderr), "1 validated, 1 passed, 0 failed"); n != 2 {
			t.Fatalf("want one Sourcemeta success per schema group, got %d:\n%s", n, res)
		}

		res, rep = run(t, broken, "schepherd.toml", nil, "config/company.json")
		res.wantCode(t, 2)
		examplesWantTasks(t, rep, "batch", 2, []consumersTask{
			{schemaID: "company-config", status: "failed", exitCode: 2, files: []string{filepath.Join(broken, "config", "company.json")}},
		})

		if !strings.Contains(string(res.Stdout)+string(res.Stderr), "Schema validation failure") {
			t.Fatalf("Sourcemeta did not report the invalid company configuration:\n%s", res)
		}
	})

	t.Run("the batch example hands the consumer the base's settings", func(t *testing.T) {
		t.Parallel()

		logDir := t.TempDir()
		res, _ := run(t, valid, "schepherd.toml", []string{"JSONSCHEMA_BIN=" + binPath("testconsumer"), "TC_LOG_DIR=" + logDir}, "config/company.json")
		res.ok(t)

		recs := consumersRecords(t, logDir)
		if len(recs) != 1 {
			t.Fatalf("want one consumer process, got %+v", recs)
		}

		rec := recs[0]
		if len(rec.Argv) != 4 || rec.Argv[1] != "validate" || !filepath.IsAbs(rec.Argv[2]) || rec.Argv[3] != filepath.Join(valid, "config", "company.json") {
			t.Errorf("argv %q, want validate <schema> <company.json>", rec.Argv)
		}

		if consumersRealPath(t, rec.Cwd) != consumersRealPath(t, valid) {
			t.Errorf("cwd %s, want the workspace %s (runner.cwd of the base)", rec.Cwd, valid)
		}

		for k, v := range map[string]string{"NO_COLOR": "1", "SCHEMA_ID": "company-config"} {
			if got, ok := rec.Env[k]; !ok || got != v {
				t.Errorf("consumer %s=%q (set %v), want %q", k, got, ok, v)
			}
		}
	})

	t.Run("sourcemeta stdin example", func(t *testing.T) {
		t.Parallel()

		res, rep := run(t, valid, "stdin.schepherd.toml", nil, "alpha.json", ".github/workflows/ci.yml")
		res.ok(t)
		examplesWantTasks(t, rep, "stdin", 0, []consumersTask{
			{schemaID: "alpha", status: "ok", files: []string{filepath.Join(valid, "alpha.json")}},
			{schemaID: "alpha", status: "ok", files: []string{filepath.Join(valid, ".github", "workflows", "ci.yml")}},
		})

		if n := strings.Count(string(res.Stdout)+string(res.Stderr), "1 validated, 1 passed, 0 failed"); n != 2 {
			t.Fatalf("want one Sourcemeta success per streamed file, got %d:\n%s", n, res)
		}

		res, rep = run(t, broken, "stdin.schepherd.toml", nil, "alpha.json")
		res.wantCode(t, 2)
		examplesWantTasks(t, rep, "stdin", 2, []consumersTask{
			{schemaID: "alpha", status: "failed", exitCode: 2, files: []string{filepath.Join(broken, "alpha.json")}},
		})
	})

	t.Run("check-jsonschema example in the validators container", func(t *testing.T) {
		t.Parallel()

		// The container runs offline on the host sandbox's cache.
		for _, id := range []string{"alpha", "beta"} {
			cli(t, runOpts{Sandbox: sb, Env: env}, "--config", filepath.Join(examples, "check-jsonschema.schepherd.toml"), "path", id).ok(t)
		}

		container := func(t *testing.T, workdir string, args ...string) (result, consumersReport) {
			t.Helper()

			reportDir := t.TempDir()
			full := append([]string{
				consumersContainerBin, "--config", examplesContainerDir + "/check-jsonschema.schepherd.toml", "--offline",
				"--cache-dir", consumersContainerCache, "run", "--report", consumersContainerReport + "/report.json", "--",
			}, args...)

			res := dockerRun(t, validatorsImage(t), dockerOpts{
				Mounts: []mount{
					binMount(),
					{Host: examples, Container: examplesContainerDir},
					{Host: sb.CacheDir, Container: consumersContainerCache, Writable: true},
					{Host: valid, Container: examplesContainerValid},
					{Host: broken, Container: examplesContainerBroken},
					{Host: reportDir, Container: consumersContainerReport, Writable: true},
				},
				Network: "none",
				Workdir: workdir,
				Env: []string{
					"SCHEPHERD_REPOSITORY=" + repo, "SCHEPHERD_CATALOG=" + catalog,
					"CHECK_JSONSCHEMA_BIN=" + consumersCheckJSONSchemaBin,
				},
			}, full...)

			return res, decodeJSON[consumersReport](t, readFile(t, filepath.Join(reportDir, "report.json")))
		}

		res, rep := container(t, examplesContainerValid, "alpha.json", "config/app/settings.toml")
		res.ok(t)
		examplesWantTasks(t, rep, "batch", 0, []consumersTask{
			{schemaID: "alpha", status: "ok", files: []string{examplesContainerValid + "/alpha.json"}},
			{schemaID: "beta", status: "ok", files: []string{examplesContainerValid + "/config/app/settings.toml"}},
		})

		if n := strings.Count(string(res.Stdout), "ok -- validation done"); n != 2 {
			t.Fatalf("want one check-jsonschema success per schema group, got %d:\n%s", n, res)
		}

		res, rep = container(t, examplesContainerBroken, "alpha.json")
		res.wantCode(t, 1)
		examplesWantTasks(t, rep, "batch", 1, []consumersTask{
			{schemaID: "alpha", status: "failed", exitCode: 1, files: []string{examplesContainerBroken + "/alpha.json"}},
		})
	})
}

// examplesCheckMerge runs `config check --json` for one example and compares
// the effective configuration with the example and its base as read from
// the files: runner keys the example leaves out come from the base, both
// runner.env tables merge, and the catalog pin comes from the environment.
func examplesCheckMerge(t *testing.T, sb *sandbox, dir, name string, env []string, repo, catalog string) {
	t.Helper()

	example := examplesRead(t, filepath.Join(dir, name))
	if !slices.Equal(example.Extends, []string{"./base.schepherd.toml"}) {
		t.Fatalf("%s extends %q, want the shipped ./base.schepherd.toml", name, example.Extends)
	}

	base := examplesRead(t, filepath.Join(dir, "base.schepherd.toml"))

	res := cli(t, runOpts{Sandbox: sb, Env: env}, "--config", filepath.Join(dir, name), "config", "check", "--json").ok(t)
	got := decodeJSON[examplesSummary](t, res.Stdout)

	if len(got.Files) != 2 || filepath.Base(got.Files[0]) != "base.schepherd.toml" || filepath.Base(got.Files[1]) != name {
		t.Errorf("%s: files %q, want the base, then the example", name, got.Files)
	}

	if got.Repository != repo || got.Catalog != catalog {
		t.Errorf("%s: catalog %s@%s, want %s@%s", name, got.Repository, got.Catalog, repo, catalog)
	}

	pick := func(child, parent string) string {
		if child != "" {
			return child
		}

		return parent
	}

	want := examplesFile{}
	want.Runner.Mode = pick(example.Runner.Mode, base.Runner.Mode)
	want.Runner.Command = pick(example.Runner.Command, base.Runner.Command)
	want.Runner.Cwd = pick(example.Runner.Cwd, base.Runner.Cwd)
	want.Runner.Args = example.Runner.Args
	want.Runner.Env = maps.Clone(base.Runner.Env)
	maps.Copy(want.Runner.Env, example.Runner.Env)

	r := got.Runner
	if r.Mode != want.Runner.Mode || r.Command != want.Runner.Command || r.Cwd != want.Runner.Cwd ||
		!slices.Equal(r.Args, want.Runner.Args) || !maps.Equal(r.Env, want.Runner.Env) {
		t.Errorf("%s: runner %+v, want mode %q, command %q, cwd %q, args %q, env %v",
			name, r, want.Runner.Mode, want.Runner.Command, want.Runner.Cwd, want.Runner.Args, want.Runner.Env)
	}

	timeout := pick(example.Runner.Timeout, base.Runner.Timeout)
	if gotTimeout, err := time.ParseDuration(r.Timeout); err != nil || timeout == "" || gotTimeout != examplesDuration(t, timeout) {
		t.Errorf("%s: runner timeout %q, want the base's %q", name, r.Timeout, timeout)
	}

	if base.Runner.Jobs != 0 && example.Runner.Jobs == 0 && r.Jobs != base.Runner.Jobs {
		t.Errorf("%s: runner jobs %d, want the base's %d", name, r.Jobs, base.Runner.Jobs)
	}

	if f := base.Runner.FailFast; f != nil && example.Runner.FailFast == nil && r.FailFast != *f {
		t.Errorf("%s: runner fail_fast %v, want the base's %v", name, r.FailFast, *f)
	}

	if i := base.Runner.InheritEnv; i != nil && example.Runner.InheritEnv == nil && r.InheritEnv != *i {
		t.Errorf("%s: runner inherit_env %v, want the base's %v", name, r.InheritEnv, *i)
	}

	if len(got.Mappings) != len(example.Mappings) {
		t.Fatalf("%s: mappings %+v, want %+v", name, got.Mappings, example.Mappings)
	}

	for i, m := range example.Mappings {
		if got.Mappings[i].Schema != m.Schema || !slices.Equal(got.Mappings[i].FileMatch, m.FileMatch) {
			t.Errorf("%s: mapping %d is %+v, want %+v", name, i, got.Mappings[i], m)
		}
	}
}

func examplesRead(t *testing.T, path string) examplesFile {
	t.Helper()

	var f examplesFile
	if err := toml.Unmarshal(readFile(t, path), &f); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	return f
}

func examplesDuration(t *testing.T, s string) time.Duration {
	t.Helper()

	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatalf("duration %q: %v", s, err)
	}

	return d
}

// examplesWantTasks checks a run report: its mode and exit status and
// exactly the expected tasks, each with an artifact of the run's catalog.
func examplesWantTasks(t *testing.T, rep consumersReport, mode string, code int, want []consumersTask) {
	t.Helper()

	if rep.ReportVersion != 1 || rep.Mode != mode || rep.ExitCode != code || len(rep.Tasks) != len(want) {
		t.Fatalf("report %+v: want version 1, mode %s, exitCode %d and %d tasks", rep, mode, code, len(want))
	}

	for i, w := range want {
		got := rep.Tasks[i]
		if got.SchemaID != w.schemaID || got.Status != w.status || got.ExitCode != w.exitCode || !slices.Equal(got.Files, w.files) ||
			!strings.Contains(got.SchemaRef, "@sha256:") {
			t.Fatalf("report task %d is %+v, want %s, status %q, exitCode %d, files %q", i, got, w.schemaID, w.status, w.exitCode, w.files)
		}
	}
}

// examplesAppend appends text to a file.
func examplesAppend(t *testing.T, path, text string) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.WriteString(text)
	if cerr := f.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		t.Fatal(err)
	}
}
