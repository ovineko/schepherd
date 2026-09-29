package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/interp"
)

// CheckRunnerEnv reports every ${NAME} in runner.command, runner.args,
// runner.cwd and runner.env that lookup cannot resolve, and a runner.command
// or runner.cwd that expands to an empty string, so that both fail before any
// consumer starts, like a literal empty value does.
func (c *Config) CheckRunnerEnv(lookup interp.LookupFunc) error {
	if lookup == nil {
		lookup = unsetLookup
	}

	var missing []string

	for _, f := range c.runnerFields() {
		tpl, err := interp.Parse(f.raw)
		if err != nil {
			return fault.Wrap(fault.Usage, err, "%s", f.name)
		}

		for _, name := range tpl.EnvVars() {
			if _, ok := lookup(name); !ok {
				missing = append(missing, fmt.Sprintf("${%s} in %s", name, f.name))
			}
		}
	}

	if len(missing) > 0 {
		return fault.New(fault.Usage, "runner uses unset environment variables: %s", strings.Join(missing, ", "))
	}

	for _, f := range []runnerField{{"runner.command", c.Runner.Command}, {"runner.cwd", c.Runner.Cwd}} {
		if expandsToEmpty(f.raw, lookup) {
			return fault.New(fault.Usage, "%s expands to an empty string", f.name)
		}
	}

	return nil
}

// expandsToEmpty reports whether a non-empty template without placeholders
// expands to nothing; {workspace} and {cache} are never empty.
func expandsToEmpty(raw string, lookup interp.LookupFunc) bool {
	tpl, err := interp.Parse(raw)
	if raw == "" || err != nil || len(tpl.Placeholders()) > 0 {
		return false
	}

	value, err := tpl.Expand(lookup, nil)

	return err == nil && value == ""
}

type runnerField struct {
	name string
	raw  string
}

func (c *Config) runnerFields() []runnerField {
	spec := &c.Runner
	fields := make([]runnerField, 0, 2+len(spec.Args)+len(spec.Env))
	fields = append(fields, runnerField{"runner.command", spec.Command}, runnerField{"runner.cwd", spec.Cwd})

	for i, arg := range spec.Args {
		fields = append(fields, runnerField{fmt.Sprintf("runner.args[%d]", i), arg})
	}

	for _, name := range slices.Sorted(maps.Keys(spec.Env)) {
		fields = append(fields, runnerField{joinKey("runner.env", name), spec.Env[name]})
	}

	return fields
}

type summary struct {
	Registries map[string]registrySummary `json:"registries"`
	Schemas    map[string]schemaSummary   `json:"schemas"`
	Repository string                     `json:"repository"`
	Catalog    string                     `json:"catalog"`
	Workspace  string                     `json:"workspace"`
	CacheDir   string                     `json:"cacheDir"`
	Timeout    string                     `json:"timeout"`
	Files      []string                   `json:"files"`
	Mappings   []mappingSummary           `json:"mappings"`
	Runner     runnerSummary              `json:"runner"`
	Limits     limitsSummary              `json:"limits"`
	Offline    bool                       `json:"offline"`
}

type limitsSummary struct {
	MaxManifestBytes  int64 `json:"maxManifestBytes"`
	MaxCatalogBytes   int64 `json:"maxCatalogBytes"`
	MaxCatalogEntries int   `json:"maxCatalogEntries"`
	MaxPayloadBytes   int64 `json:"maxPayloadBytes"`
	MaxSchemaBytes    int64 `json:"maxSchemaBytes"`
}

type registrySummary struct {
	CAFile          string `json:"caFile,omitempty"`
	CredentialsFile string `json:"credentialsFile,omitempty"`
	PlainHTTP       bool   `json:"plainHttp"`
}

type runnerSummary struct {
	Env          map[string]string `json:"env"`
	Mode         string            `json:"mode"`
	Command      string            `json:"command"`
	Cwd          string            `json:"cwd"`
	CwdBase      string            `json:"cwdBase"`
	Timeout      string            `json:"timeout"`
	Args         []string          `json:"args"`
	Jobs         int               `json:"jobs"`
	MaxArgsBytes int               `json:"maxArgsBytes"`
	Configured   bool              `json:"configured"`
	FailFast     bool              `json:"failFast"`
	InheritEnv   bool              `json:"inheritEnv"`
}

type schemaSummary struct {
	Path       string   `json:"path"`
	DeclaredIn string   `json:"declaredIn"`
	FileMatch  []string `json:"fileMatch"`
}

type mappingSummary struct {
	Schema    string   `json:"schema"`
	FileMatch []string `json:"fileMatch"`
}

// Summary returns the effective configuration as a value for encoding/json,
// as printed by "config check --json". Runner fields are shown as written:
// templates are never expanded here, so values of variables they reference
// cannot leak. An empty cacheDir means the platform default.
func (c *Config) Summary() any {
	s := summary{
		Files:      nonNil(c.Files),
		Workspace:  c.Workspace,
		CacheDir:   c.CacheDir,
		Offline:    c.Offline,
		Timeout:    c.Timeout.String(),
		Repository: c.Repository,
		Catalog:    c.Catalog,
		Limits: limitsSummary{
			MaxManifestBytes:  c.Limits.MaxManifestBytes,
			MaxCatalogBytes:   c.Limits.MaxCatalogBytes,
			MaxCatalogEntries: c.Limits.MaxCatalogEntries,
			MaxPayloadBytes:   c.Limits.MaxPayloadBytes,
			MaxSchemaBytes:    c.Limits.MaxSchemaBytes,
		},
		Registries: make(map[string]registrySummary, len(c.Registries)),
		Mappings:   make([]mappingSummary, 0, len(c.Mappings)),
		Schemas:    make(map[string]schemaSummary, len(c.LocalSchemas)),
		Runner: runnerSummary{
			Configured:   c.RunnerConfigured,
			Mode:         string(c.Runner.Mode),
			Command:      c.Runner.Command,
			Args:         nonNil(c.Runner.Args),
			Cwd:          c.Runner.Cwd,
			CwdBase:      c.Runner.CwdBase,
			Timeout:      c.Runner.Timeout.String(),
			Jobs:         c.Runner.Jobs,
			FailFast:     c.Runner.FailFast,
			InheritEnv:   c.Runner.InheritEnv,
			MaxArgsBytes: c.Runner.MaxArgsBytes,
			Env:          maps.Clone(c.Runner.Env),
		},
	}

	if s.Runner.Env == nil {
		s.Runner.Env = map[string]string{}
	}

	for host, reg := range c.Registries {
		s.Registries[host] = registrySummary(reg)
	}

	for _, m := range c.Mappings {
		s.Mappings = append(s.Mappings, mappingSummary{Schema: m.Schema, FileMatch: nonNil(m.FileMatch)})
	}

	for _, ls := range c.LocalSchemas {
		s.Schemas[ls.ID] = schemaSummary{Path: ls.Path, DeclaredIn: ls.DeclaredIn, FileMatch: nonNil(ls.FileMatch)}
	}

	return s
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}

	return slices.Clone(list)
}
