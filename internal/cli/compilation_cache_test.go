package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/config"
	"github.com/ovineko/schepherd/internal/processor"
	"github.com/ovineko/schepherd/internal/runner"
)

func TestCustomProcessorLocalSchemasNeverOpenCompilerCache(t *testing.T) {
	for _, cacheState := range []string{"absent", "file", "prepared without service directories"} {
		t.Run(cacheState, func(t *testing.T) {
			p := newLocalProject(t, companySection)
			legacy := consumerRunner(t, p.marker, 0)
			custom := strings.Replace(legacy, "[runner]", "[processors.fixture]\ntype = \"custom\"\nmode = \"batch\"", 1)
			custom = strings.Replace(custom, "[runner.env]", "[processors.fixture.env]", 1)
			writeTestFile(t, p.config, "config_version = 1\n\n"+companySection+"\n"+custom)
			var before any
			switch cacheState {
			case "file":
				writeTestFile(t, p.cache, "not a cache directory")
			case "prepared without service directories":
				prepared, _ := seedOfflineMaterializeCache(t)
				p.cache = prepared
				for _, name := range []string{"locks", "tmp", "quarantine"} {
					if err := os.RemoveAll(filepath.Join(prepared, "v1", name)); err != nil {
						t.Fatal(err)
					}
				}
				before = offlineCacheSnapshot(t, prepared)
			}
			file := p.file("config/team.json")
			stdout, stderr, code := run(t, p.args("--offline", "run", "--json", "--", file)...)
			report := decodeRunJSON(t, stdout, stderr, code)
			if code != 0 || len(report.Tasks) != 1 || report.Tasks[0].Status != runner.StatusOK || report.Tasks[0].Processor.Type != "custom" || report.Tasks[0].Files[0] != file {
				t.Fatalf("custom local validation: code=%d report=%s", code, stdout)
			}
			marker, err := os.ReadFile(p.marker)
			if err != nil || !strings.Contains(string(marker), file) {
				t.Fatalf("consumer did not receive original input: %q %v", marker, err)
			}
			switch cacheState {
			case "absent":
				if _, err := os.Stat(p.cache); !os.IsNotExist(err) {
					t.Fatalf("custom processor created compiler cache: %v", err)
				}
			case "file":
				data, err := os.ReadFile(p.cache)
				if err != nil || string(data) != "not a cache directory" {
					t.Fatalf("custom processor changed unused cache path: %q %v", data, err)
				}
			case "prepared without service directories":
				if after := offlineCacheSnapshot(t, p.cache); !reflect.DeepEqual(before, after) {
					t.Fatalf("custom processor modified prepared cache: before=%v after=%v", before, after)
				}
			}
		})
	}
}

func TestTypedLocalSchemasDoNotOpenCompilerCache(t *testing.T) {
	for _, state := range []string{"absent", "file"} {
		t.Run(state, func(t *testing.T) {
			cachePath := filepath.Join(t.TempDir(), "cache")
			if state == "file" {
				if err := os.WriteFile(cachePath, []byte("untouched"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			src := &sources{cfg: &config.Config{
				CacheDir: cachePath,
				Offline:  true,
				Processors: map[string]processor.Definition{
					"validator": {Type: "sourcemeta"},
				},
			}}
			compiled, cleanup, err := src.compiler(map[string]runner.SchemaInfo{
				"local": {ID: "local", Origin: originLocal},
			})
			if err != nil {
				t.Fatal(err)
			}
			cleanup()
			if compiled.Cache != nil || !compiled.VerifyOnly {
				t.Fatalf("local compiler cache state: %+v", compiled)
			}
			if state == "absent" {
				if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
					t.Fatalf("typed local run created compiler cache: %v", err)
				}
			} else if data, err := os.ReadFile(cachePath); err != nil || string(data) != "untouched" {
				t.Fatalf("typed local run changed cache path: %q %v", data, err)
			}
		})
	}
}
