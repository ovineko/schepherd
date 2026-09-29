package runner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func FuzzChunkFiles(f *testing.F) {
	f.Add("a\x00bb\x00ccc", 6)
	f.Add("", 0)
	f.Add("one", 1)
	f.Add("x\x00y\x00z", -3)

	f.Fuzz(func(t *testing.T, joined string, budget int) {
		files := strings.Split(joined, "\x00")
		if len(files) > 256 {
			files = files[:256]
		}

		chunks, err := chunkFiles(files, budget)

		fits := true

		for _, file := range files {
			if argBytes(file) > budget {
				fits = false
			}
		}

		if !fits {
			if err == nil {
				t.Fatalf("files %q exceed budget %d but no error", files, budget)
			}

			return
		}

		if err != nil {
			t.Fatalf("chunkFiles(%q, %d): %v", files, budget, err)
		}

		var all []string

		for i, chunk := range chunks {
			if len(chunk) == 0 {
				t.Fatalf("chunk %d is empty", i)
			}

			if cost := argvBytes(chunk); cost > budget {
				t.Fatalf("chunk %d costs %d > %d", i, cost, budget)
			}

			if i+1 < len(chunks) && argvBytes(chunk)+argBytes(chunks[i+1][0]) <= budget {
				t.Fatalf("chunk %d was closed although the next file fits", i)
			}

			all = append(all, chunk...)
		}

		if !slices.Equal(all, files) {
			t.Fatalf("chunks %q do not preserve %q", chunks, files)
		}
	})
}

func FuzzMergeEnv(f *testing.F) {
	f.Add("A=1\x00B=2\x00A=3", "B", "x")
	f.Add("=C:=C:\\\x00PATH=/bin", "PATH", "")
	f.Add("garbage\x00\x00=", "K", "v=w")

	f.Fuzz(func(t *testing.T, base, key, value string) {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return
		}

		env := mergeEnv(strings.Split(base, "\x00"), []envVar{{key: key, value: value}})

		seen := map[string]bool{}

		for _, entry := range env {
			name, ok := envKey(entry)
			if !ok {
				t.Fatalf("entry %q has no name", entry)
			}

			folded := foldEnvKey(name)
			if seen[folded] {
				t.Fatalf("duplicate key %q in %q", name, env)
			}

			seen[folded] = true
		}

		if got, ok := lookupEnv(env, key); !ok || got != value {
			t.Fatalf("%s = %q (%v), want overlay value %q", key, got, ok, value)
		}
	})
}

func FuzzFileNamesStayLiteral(f *testing.F) {
	f.Add("plain.json")
	f.Add("{schema}")
	f.Add("${HOME} $(id) `id` ; & | > *")
	f.Add("-rf --help")
	f.Add("ünï cødé")

	exe := fuzzExecutable(f)
	root := f.TempDir()

	f.Fuzz(func(t *testing.T, name string) {
		path := filepath.Join(root, name)
		if strings.IndexByte(name, 0) >= 0 || !filepath.IsAbs(path) {
			return
		}

		schemas := map[string]SchemaInfo{"s": {ID: "s", Path: filepath.Join(root, "schema.json"), Ref: "r@sha256:0"}}
		inputs := []Input{{Path: path, SchemaID: "s"}}

		for _, spec := range []Spec{
			{Mode: ModeBatch, Args: []string{"{schema}", "{files...}"}},
			{Mode: ModePerFile, Args: []string{"--file={file}", "{file}"}, Env: map[string]string{"F": "{file}"}},
		} {
			spec.Command = exe
			spec.Timeout = DefaultTimeout
			spec.Jobs = 1

			plan, err := BuildPlan(inputs, schemas, Options{Spec: spec, Workspace: root, CacheDir: root})
			if err != nil {
				t.Fatalf("%s: %v", spec.Mode, err)
			}

			args := plan.Tasks[0].Args
			if args[len(args)-1] != filepath.Clean(path) {
				t.Fatalf("%s: last argument %q, want %q", spec.Mode, args[len(args)-1], filepath.Clean(path))
			}

			if spec.Mode == ModePerFile {
				if args[1] != "--file="+filepath.Clean(path) {
					t.Fatalf("argument %q", args[1])
				}

				if v, _ := lookupEnv(plan.Tasks[0].Env, "F"); v != filepath.Clean(path) {
					t.Fatalf("F = %q", v)
				}
			}
		}
	})
}

func fuzzExecutable(f *testing.F) string {
	f.Helper()

	exe, err := os.Executable()
	if err != nil {
		f.Fatal(err)
	}

	if strings.ContainsAny(exe, "{}") || strings.Contains(exe, "${") {
		f.Fatalf("test binary path %q cannot be used as a literal template", exe)
	}

	return exe
}
