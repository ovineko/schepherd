package runner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
)

func touch(t *testing.T, paths ...string) {
	t.Helper()

	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExecutableExtsReadConsumerPathext(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want []string
	}{
		{name: "unset", env: nil, want: defaultExts},
		{name: "empty", env: []string{"PATHEXT="}, want: defaultExts},
		{name: "only separators", env: []string{"PATHEXT=;;"}, want: defaultExts},
		{name: "normalized", env: []string{"PATHEXT=.EXE;cmd;;.Com"}, want: []string{".exe", ".cmd", ".com"}},
		{name: "key case", env: []string{"PathExt=.EXE"}, want: []string{".exe"}},
		{name: "last entry wins", env: []string{"PATHEXT=.COM", "pathext=.EXE"}, want: []string{".exe"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := executableExts(c.env); !slices.Equal(got, c.want) {
				t.Errorf("executableExts(%q) = %q, want %q", c.env, got, c.want)
			}
		})
	}
}

func TestFindExecutableAppliesPathext(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "tool.exe"), filepath.Join(dir, "tool.bat"), filepath.Join(dir, "plain"))

	if err := os.Mkdir(filepath.Join(dir, "sub.exe"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want string
		exts []string
	}{
		{name: "first extension in order", path: "tool", exts: []string{".com", ".exe", ".bat"}, want: "tool.exe"},
		{name: "order decides", path: "tool", exts: []string{".bat", ".exe"}, want: "tool.bat"},
		{name: "explicit extension used as is", path: "tool.exe", exts: []string{".com"}, want: "tool.exe"},
		{name: "no extension from the list", path: "plain", exts: []string{".exe"}},
		{name: "directory with an executable name", path: "sub", exts: []string{".exe"}},
		{name: "missing", path: "absent", exts: defaultExts},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := findExecutable(filepath.Join(dir, c.path), c.exts)

			switch {
			case c.want == "" && err == nil:
				t.Errorf("found %q, want an error", got)
			case c.want != "" && (err != nil || got != filepath.Join(dir, c.want)):
				t.Errorf("got %q (%v), want %q", got, err, filepath.Join(dir, c.want))
			}
		})
	}
}

func TestResolveExecutableOnWindows(t *testing.T) {
	root := t.TempDir()
	exeDir := filepath.Join(root, "exe")
	cmdDir := filepath.Join(root, "cmd")
	touch(t,
		filepath.Join(exeDir, "tool.exe"),
		filepath.Join(exeDir, "tool.cmd"),
		filepath.Join(cmdDir, "tool.cmd"),
		filepath.Join(root, "direct.exe"),
		filepath.Join(root, "script.CMD"),
	)

	list := func(dirs ...string) string {
		return strings.Join(dirs, string(os.PathListSeparator))
	}

	cases := []struct {
		name     string
		command  string
		pathList string
		want     string
		kind     fault.Kind
		exts     []string
	}{
		{name: "PATHEXT order prefers .exe", command: "tool", pathList: list(exeDir), exts: []string{".exe", ".cmd"}, want: filepath.Join(exeDir, "tool.exe")},
		{name: "relative and empty entries are skipped", command: "tool", pathList: list("", "exe", exeDir), exts: []string{".exe"}, want: filepath.Join(exeDir, "tool.exe")},
		{name: "absolute command without extension", command: filepath.Join(root, "direct"), exts: []string{".exe"}, want: filepath.Join(root, "direct.exe")},
		{name: "PATHEXT order prefers .cmd", command: "tool", pathList: list(exeDir), exts: []string{".cmd", ".exe"}, kind: fault.ConsumerStart},
		{name: "batch file earlier in PATH is not skipped", command: "tool", pathList: list(cmdDir, exeDir), exts: []string{".exe", ".cmd"}, kind: fault.ConsumerStart},
		{name: "explicit batch file in any case", command: filepath.Join(root, "script.CMD"), exts: defaultExts, kind: fault.ConsumerStart},
		{name: "not in PATH", command: "absent", pathList: list(exeDir), exts: defaultExts, kind: fault.ConsumerStart},
		{name: "rooted without a drive", command: `\tools\tool.exe`, exts: defaultExts, kind: fault.Usage},
		{name: "relative to a drive", command: `C:tool.exe`, exts: defaultExts, kind: fault.Usage},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveExecutable(c.command, root, c.pathList, c.exts)
			if c.want == "" {
				wantKind(t, err, c.kind)

				return
			}

			if err != nil || got != c.want {
				t.Errorf("got %q (%v), want %q", got, err, c.want)
			}
		})
	}
}

func TestMergeEnvFoldsKeyCase(t *testing.T) {
	base := []string{`Path=C:\a`, `=C:=C:\work`, `windir=C:\Windows`, `PATH=C:\dup`}
	got := mergeEnv(base, []envVar{{key: "PATH", value: `C:\b`}, {key: "PathExt", value: ".EXE"}})

	if want := []string{`PATH=C:\b`, `=C:=C:\work`, `windir=C:\Windows`, "PathExt=.EXE"}; !slices.Equal(got, want) {
		t.Fatalf("mergeEnv = %q, want %q", got, want)
	}

	for _, key := range []string{"PATH", "path", "Path"} {
		if value, ok := lookupEnv(got, key); !ok || value != `C:\b` {
			t.Errorf("lookupEnv(%q) = %q, %v", key, value, ok)
		}
	}
}

func TestBuildPlanResolvesWithConsumerPathAndPathext(t *testing.T) {
	h := newHarness(t)
	decoy := filepath.Join(h.root, "decoy")
	bin := filepath.Join(h.root, "bin")
	touch(t,
		filepath.Join(decoy, "fake-consumer.exe"),
		filepath.Join(bin, "fake-consumer.com"),
		filepath.Join(bin, "fake-consumer.exe"),
	)

	spec := h.spec(ModeBatch, "{files...}")
	spec.Command = "fake-consumer"
	spec.Env["PATH"] = bin
	spec.Env["PathExt"] = ".EXE"

	opts := h.options(spec)
	opts.Environ = append(opts.Environ, "Path="+decoy, "PATHEXT=.COM")

	plan, err := BuildPlan([]Input{{Path: h.file("in.json", ""), SchemaID: "a"}}, h.schemas("a"), opts)
	if err != nil {
		t.Fatal(err)
	}

	task := plan.Tasks[0]
	if want := filepath.Join(bin, "fake-consumer.exe"); task.Path != want {
		t.Errorf("Path = %q, want %q", task.Path, want)
	}

	for key, want := range map[string]string{"PATH": bin, "PATHEXT": ".EXE"} {
		var entries []string

		for _, entry := range task.Env {
			if name, ok := envKey(entry); ok && strings.EqualFold(name, key) {
				entries = append(entries, entry)
			}
		}

		if len(entries) != 1 || !strings.HasSuffix(entries[0], "="+want) {
			t.Errorf("%s entries in the consumer environment = %q, want one with value %q", key, entries, want)
		}
	}
}
