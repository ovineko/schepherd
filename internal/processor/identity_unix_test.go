//go:build !windows

package processor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ovineko/schepherd/internal/runner"
)

func TestCheckJSONSchemaBackendRelocationPreservesRecipes(t *testing.T) {
	root := t.TempDir()
	frontend := filepath.Join(root, "frontend")
	if err := os.WriteFile(frontend, []byte("#!/bin/sh\nif [ \"$1\" = identity ]; then printf '{\"backend\":\"%s\"}' \"$(cat \"$CHECK_JSONSCHEMA_BIN\")\"; exit 0; fi\nexit 9\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	backendA := filepath.Join(root, "store-a", "bin")
	backendB := filepath.Join(root, "store-b", "bin")
	for _, path := range []string{backendA, backendB} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(backendA, []byte("same frontend dependencies"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backendB, []byte("same frontend dependencies"), 0o600); err != nil {
		t.Fatal(err)
	}
	definition := Definition{
		Type:   "check-jsonschema",
		Output: "check-jsonschema-json",
		Spec:   runner.Spec{Env: map[string]string{"CHECK_JSONSCHEMA_BIN": "${CHECK_JSONSCHEMA_BIN}"}},
	}
	definition.Spec.Args = DefaultArgs(definition.Type)
	task := func(backend string) runner.Task {
		return runner.Task{Path: frontend, Dir: root, Env: []string{"CHECK_JSONSCHEMA_BIN=" + backend}}
	}
	toolA, settingsA, err := Identity(t.Context(), definition, task(backendA))
	if err != nil {
		t.Fatal(err)
	}
	toolB, settingsB, err := Identity(t.Context(), definition, task(backendB))
	if err != nil {
		t.Fatal(err)
	}
	if toolA != toolB || settingsA != settingsB {
		t.Fatalf("relocated backend changed identity: tool %s/%s settings %s/%s", toolA, toolB, settingsA, settingsB)
	}
	compiler := &Compiler{}
	keyA, err := compiler.key(context.Background(), definition, task(backendA), []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := compiler.key(context.Background(), definition, task(backendB), []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	if keyA != keyB {
		t.Fatalf("relocated backend changed compilation recipe: %s != %s", keyA, keyB)
	}
	if err := os.WriteFile(backendB, []byte("changed frontend dependencies"), 0o600); err != nil {
		t.Fatal(err)
	}
	toolChanged, settingsChanged, err := Identity(t.Context(), definition, task(backendB))
	if err != nil {
		t.Fatal(err)
	}
	if toolChanged == toolA || settingsChanged == settingsA {
		t.Fatalf("changed backend identity was not reflected: tool %s/%s settings %s/%s", toolA, toolChanged, settingsA, settingsChanged)
	}
	compilerChanged := &Compiler{}
	keyChanged, err := compilerChanged.key(context.Background(), definition, task(backendB), []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	if keyChanged == keyA {
		t.Fatalf("changed backend identity reused compilation recipe %s", keyA)
	}
}
