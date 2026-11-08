package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// protectedEnvironments must each require a reviewer and allow deployments
// only from their protected refs in the repository settings (docs/security.md);
// the workflows cannot prove that, so this list names what must be protected.
var protectedEnvironments = []string{"github-pages", "npm", "pypi", "release", "rubygems", "schemas-publish"}

var secretReference = regexp.MustCompile(`secrets\.([A-Za-z0-9_]+)`)

type workflowJob struct {
	Environment yaml.Node `yaml:"environment"`
	Permissions any       `yaml:"permissions"`
	Uses        string    `yaml:"uses"`
	Steps       []struct {
		If   string         `yaml:"if"`
		Uses string         `yaml:"uses"`
		With map[string]any `yaml:"with"`
	} `yaml:"steps"`
}

type workflowFile struct {
	On          yaml.Node            `yaml:"on"`
	Concurrency yaml.Node            `yaml:"concurrency"`
	Permissions any                  `yaml:"permissions"`
	Jobs        map[string]yaml.Node `yaml:"jobs"`
}

func readWorkflows(t *testing.T) map[string]workflowFile {
	t.Helper()

	files, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows: %v", err)
	}

	workflows := map[string]workflowFile{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var wf workflowFile
		if err := yaml.Unmarshal(data, &wf); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		workflows[filepath.Base(file)] = wf
	}
	return workflows
}

func environmentName(n yaml.Node) string {
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "name" {
				return n.Content[i+1].Value
			}
		}
	}
	return n.Value
}

// grantsWrite reports whether a permissions value grants any write access:
// write-all, or a scope set to write.
func grantsWrite(permissions any) bool {
	switch p := permissions.(type) {
	case string:
		return p == "write-all"
	case map[string]any:
		for _, level := range p {
			if level == "write" {
				return true
			}
		}
	}
	return false
}

// TestWorkflowsWriteOnlyInProtectedEnvironments keeps every token that can
// publish (a write permission, id-token included) and every stored secret in
// jobs of a protected environment, so each publication waits for a reviewer.
func TestWorkflowsWriteOnlyInProtectedEnvironments(t *testing.T) {
	for name, wf := range readWorkflows(t) {
		if grantsWrite(wf.Permissions) {
			t.Errorf("%s grants write permissions to every job; grant them per job", name)
		}
		for jobName, node := range wf.Jobs {
			var job workflowJob
			if err := node.Decode(&job); err != nil {
				t.Fatalf("%s: job %s: %v", name, jobName, err)
			}
			raw, err := yaml.Marshal(&node)
			if err != nil {
				t.Fatal(err)
			}
			var secrets []string
			for _, m := range secretReference.FindAllStringSubmatch(string(raw), -1) {
				if m[1] != "GITHUB_TOKEN" {
					secrets = append(secrets, m[1])
				}
			}
			if !grantsWrite(job.Permissions) && len(secrets) == 0 {
				continue
			}
			if env := environmentName(job.Environment); !slices.Contains(protectedEnvironments, env) {
				t.Errorf("%s: job %s has write permissions or secrets %v outside a protected environment (environment %q, want one of %v)", name, jobName, secrets, env, protectedEnvironments)
			}
		}
	}
}

// TestGoReleaserVersionMatchesTheTaskfile keeps the GoReleaser of CI and the
// release workflow equal to the one `task tools` installs.
func TestGoReleaserVersionMatchesTheTaskfile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "Taskfile.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var taskfile struct {
		Vars map[string]any `yaml:"vars"`
	}
	if err := yaml.Unmarshal(data, &taskfile); err != nil {
		t.Fatal(err)
	}
	want, _ := taskfile.Vars["GORELEASER_VERSION"].(string)
	if want == "" {
		t.Fatal("Taskfile.yaml has no GORELEASER_VERSION")
	}
	found := 0
	for name, wf := range readWorkflows(t) {
		for jobName, node := range wf.Jobs {
			var job workflowJob
			if err := node.Decode(&job); err != nil {
				t.Fatalf("%s: job %s: %v", name, jobName, err)
			}
			for _, step := range job.Steps {
				if !strings.HasPrefix(step.Uses, "goreleaser/goreleaser-action@") {
					continue
				}
				found++
				if got := step.With["version"]; got != want {
					t.Errorf("%s: job %s runs GoReleaser %v, the Taskfile installs %s", name, jobName, got, want)
				}
			}
		}
	}
	if found == 0 {
		t.Error("no workflow runs goreleaser/goreleaser-action")
	}
}

func TestReleaseRehearsalWorkflowIsReadOnlyAndReusesCI(t *testing.T) {
	workflows := readWorkflows(t)
	rehearsal, ok := workflows["release-rehearsal.yml"]
	if !ok {
		t.Fatal("release rehearsal workflow is missing")
	}
	var triggers map[string]any
	if err := rehearsal.On.Decode(&triggers); err != nil {
		t.Fatal(err)
	}
	if _, manual := triggers["workflow_dispatch"]; !manual || len(triggers) != 1 {
		t.Fatalf("rehearsal must run only on manual dispatch: %#v", triggers)
	}
	permissions, ok := rehearsal.Permissions.(map[string]any)
	if !ok || len(permissions) != 1 || permissions["contents"] != "read" {
		t.Fatalf("rehearsal permissions must be contents: read only: %#v", rehearsal.Permissions)
	}
	if len(rehearsal.Jobs) != 1 {
		t.Fatalf("rehearsal must invoke only the existing mandatory CI: %d jobs", len(rehearsal.Jobs))
	}
	for _, node := range rehearsal.Jobs {
		var caller workflowJob
		if err := node.Decode(&caller); err != nil {
			t.Fatal(err)
		}
		if caller.Uses != "./.github/workflows/ci.yml" {
			t.Fatalf("rehearsal does not reuse mandatory CI: %q", caller.Uses)
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			if key := node.Content[i].Value; key != "name" && key != "uses" {
				t.Errorf("rehearsal caller must not add credentials, conditions or custom execution: %s", key)
			}
		}
	}
	var concurrency struct {
		Group string `yaml:"group"`
	}
	ciConcurrency := workflows["ci.yml"].Concurrency
	if err := ciConcurrency.Decode(&concurrency); err != nil {
		t.Fatal(err)
	}
	if group := concurrency.Group; !strings.Contains(group, "${{ github.workflow }}") || !strings.Contains(group, "${{ github.ref }}") {
		t.Errorf("CI concurrency must isolate different caller workflows on the same ref: %q", group)
	}
}

func TestReleaseRehearsalRetainsPackagesOnThePackagingRunner(t *testing.T) {
	ci := readWorkflows(t)["ci.yml"]
	var packaging workflowJob
	node := ci.Jobs["packaging"]
	if err := node.Decode(&packaging); err != nil {
		t.Fatal(err)
	}
	for _, step := range packaging.Steps {
		if step.With["name"] != "release-rehearsal-packages" {
			continue
		}
		if !strings.HasPrefix(step.Uses, "actions/upload-artifact@") || step.If != "github.event_name == 'workflow_dispatch'" {
			t.Fatalf("verified packages must be uploaded only on manual dispatch: %+v", step)
		}
		path, _ := step.With["path"].(string)
		if paths := strings.Split(strings.TrimSpace(path), "\n"); !slices.Equal(paths, []string{"dist/", "${{ runner.temp }}/packages/"}) {
			t.Errorf("artifact must retain this runner's verified dist and package outputs: %q", path)
		}
		if step.With["if-no-files-found"] != "error" {
			t.Error("missing rehearsal artifacts must fail the job")
		}
		return
	}
	t.Fatal("packaging job does not retain the verified rehearsal packages")
}
