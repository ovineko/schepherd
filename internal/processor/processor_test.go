package processor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/runner"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && (os.Args[1] == "probe" || os.Args[1] == "compile") {
		switch os.Args[2] {
		case "supported":
			fmt.Print(`{"supported":true}`)
			os.Exit(0)
		case "unsupported":
			fmt.Print(`{"supported":false,"reason":"unsupported dialect"}`)
			os.Exit(0)
		case "crash":
			fmt.Fprint(os.Stderr, "failed to initialize")
			os.Exit(1)
		case "native-unsupported":
			os.Exit(3)
		case "invalid-schema":
			os.Exit(4)
		default:
			fmt.Print(`{}`)
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func TestProbeDistinguishesUnsupportedAndInfrastructure(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	task := runner.Task{Path: executable, Dir: t.TempDir(), Env: []string{"GOCOVERDIR=" + t.TempDir(), "GORACE=atexit_sleep_ms=0"}}
	cases := []struct {
		kind, schema string
		unsupported  bool
		fault        fault.Kind
	}{
		{"ajv", "supported", false, fault.Internal},
		{"ajv", "unsupported", true, fault.Internal},
		{"ajv", "crash", false, fault.ConsumerStart},
		{"ajv", "malformed", false, fault.ConsumerStart},
		{"sourcemeta", "native-unsupported", true, fault.Internal},
		{"sourcemeta", "invalid-schema", true, fault.Internal},
		{"ajv", "invalid-schema", true, fault.Internal},
	}
	for _, tc := range cases {
		t.Run(tc.kind+tc.schema, func(t *testing.T) {
			definition := Definition{Type: tc.kind, Spec: runner.Spec{Name: "validator"}}
			reason, err := Probe(context.Background(), definition, task, tc.schema)
			if tc.unsupported {
				if err != nil || reason == "" {
					t.Fatalf("wanted incompatibility: %q %v", reason, err)
				}
				return
			}
			if tc.schema == "supported" {
				if err != nil || reason != "" {
					t.Fatalf("supported: %q %v", reason, err)
				}
				return
			}
			if err == nil || fault.KindOf(err) != tc.fault {
				t.Fatalf("wanted %v: %q %v", tc.fault, reason, err)
			}
		})
	}
	task.Path = filepath.Join(t.TempDir(), "absent")
	_, err = Probe(context.Background(), Definition{Type: "ajv"}, task, "supported")
	if fault.KindOf(err) != fault.ConsumerStart {
		t.Fatalf("missing executable: %v", err)
	}
}

func TestFormatsRequireExplicitConversion(t *testing.T) {
	native := Definition{Type: "sourcemeta"}
	if ok, _ := native.Accepts("input.toml"); ok {
		t.Fatal("native Sourcemeta unexpectedly accepts TOML")
	}
	if ok, _ := native.Accepts("input"); ok {
		t.Fatal("unknown format silently accepted")
	}
	convert := Definition{Type: "sourcemeta", Convert: "yq", Formats: []string{"toml"}}
	if ok, conversion := convert.Accepts("input.toml"); !ok || !conversion {
		t.Fatal("explicit TOML conversion not selected")
	}
	custom := Definition{Type: "custom", Formats: []string{"json"}}
	if ok, _ := custom.Accepts("input.yaml"); ok {
		t.Fatal("custom allowlist ignored")
	}
	if ok, _ := custom.Accepts("input.json"); !ok {
		t.Fatal("custom allowlist rejects JSON")
	}
}
