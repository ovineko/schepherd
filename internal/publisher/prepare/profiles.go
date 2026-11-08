package prepare

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/config"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/match"
	"github.com/ovineko/schepherd/internal/processor"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/internal/runner"
)

type profileRefreshError struct{ source string }

func (e *profileRefreshError) Error() string {
	return "profile cache miss requires preparing " + e.source
}

func profileSet(ctx context.Context, opts Options, entries []Entry, schemas map[string][]byte, previous *state.State) error {
	cfg, err := loadProcessorConfig(opts.ProcessorConfig)
	if err != nil {
		return err
	}
	canonical, err := canonicalProcessors(cfg)
	if err != nil {
		return err
	}
	for _, name := range cfg.ProcessorOrder {
		definition := cfg.Processors[name]
		if definition.Type != "custom" && canonical[definition.Type] == name && processor.InputDependent(definition) {
			return fault.New(fault.Usage, "--processors-config processor %s (%s) is input-dependent and cannot publish compatibility profiles", name, definition.Type)
		}
	}
	for i := range entries {
		if err := profileEntry(ctx, cfg, canonical, &entries[i], schemas, previous); err != nil {
			return err
		}
	}
	return nil
}

func loadProcessorConfig(path string) (*config.Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "get publisher working directory")
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "publisher working directory")
	}
	cfg, err := config.Load(config.LoadOptions{Path: path, Cwd: cwd, Lookup: env.Lookup, WithoutCatalog: true})
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "load --processors-config %s", path)
	}
	return cfg, nil
}

func canonicalProcessors(cfg *config.Config) (map[string]string, error) {
	configured := map[string]string{"sourcemeta": "", "check-jsonschema": "", "ajv": ""}
	for _, name := range cfg.ProcessorOrder {
		kind := cfg.Processors[name].Type
		if _, required := configured[kind]; !required {
			continue
		}
		if previous := configured[kind]; previous != "" {
			if !sameProcessorDefinition(cfg.Processors[previous], cfg.Processors[name]) {
				return nil, fault.New(fault.Usage, "--processors-config defines ambiguous %s processors (%s and %s)", kind, previous, name)
			}
			continue
		}
		configured[kind] = name
	}
	for _, kind := range []string{"sourcemeta", "check-jsonschema", "ajv"} {
		if configured[kind] == "" {
			return nil, fault.New(fault.Usage, "--processors-config must define a %s processor", kind)
		}
	}
	return configured, nil
}

func profileEntry(ctx context.Context, cfg *config.Config, canonical map[string]string, entry *Entry, schemas map[string][]byte, previous *state.State) error {
	schema, hasSchema := schemas[entry.ID]
	metadata, err := metadataForEntry(entry, schema, hasSchema, previous)
	if err != nil {
		return err
	}
	for _, name := range cfg.ProcessorOrder {
		definition := cfg.Processors[name]
		if definition.Type == "custom" || canonical[definition.Type] != name {
			continue
		}
		profile, err := profileProcessor(ctx, cfg, entry, schema, hasSchema, metadata, previous, definition)
		if err != nil {
			return err
		}
		metadata.Profiles = append(metadata.Profiles, profile)
	}
	entry.Metadata = metadata
	return nil
}

func metadataForEntry(entry *Entry, schema []byte, hasSchema bool, previous *state.State) (*catalog.Metadata, error) {
	if hasSchema {
		return &catalog.Metadata{SchemaDigest: digest.FromBytes(schema), Profiles: []catalog.Profile{}, SourceFormats: sourceFormats(entry.FileMatch)}, nil
	}
	if rec, ok := lookupPublisherState(previous, entry.ID); ok && rec.Entry.Metadata != nil && rec.Entry.Metadata.SchemaDigest == entry.ContentDigest {
		return &catalog.Metadata{SchemaDigest: rec.Entry.Metadata.SchemaDigest, Profiles: []catalog.Profile{}, SourceFormats: sourceFormats(entry.FileMatch)}, nil
	}
	return nil, &profileRefreshError{source: entry.Provenance.Source}
}

func profileProcessor(ctx context.Context, cfg *config.Config, entry *Entry, schema []byte, hasSchema bool, metadata *catalog.Metadata, previous *state.State, definition processor.Definition) (catalog.Profile, error) {
	dir, err := os.MkdirTemp("", "schepherd-profile-")
	if err != nil {
		return catalog.Profile{}, fault.Wrap(fault.Internal, err, "create processor probe directory")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	task, schemaPath, err := buildProbeTask(cfg, entry.ID, schema, hasSchema, dir, definition)
	if err != nil {
		return catalog.Profile{}, err
	}
	tool, settings, err := processor.Identity(ctx, definition, task)
	if err != nil {
		return catalog.Profile{}, fault.Wrap(fault.ConsumerStart, err, "identify %s processor", definition.Type)
	}
	profile := catalog.Profile{Processor: definition.Type, Tool: tool, SettingsDigest: settings, Result: "supported"}
	if old, ok := cachedProfile(previous, entry.ID, metadata.SchemaDigest, definition.Type, tool, settings); ok {
		return old, nil
	}
	if !hasSchema {
		return catalog.Profile{}, &profileRefreshError{source: entry.Provenance.Source}
	}
	reason, err := processor.Probe(ctx, definition, task, schemaPath)
	if err != nil {
		return catalog.Profile{}, fault.Wrap(fault.ConsumerStart, err, "probe %s processor", definition.Type)
	}
	if reason != "" {
		reason = processor.NormalizeDiagnosticPaths(reason,
			processor.PathSubstitution{Path: schemaPath, Token: "{schema}"},
			processor.PathSubstitution{Path: task.Path, Token: "{processor}"},
			processor.PathSubstitution{Path: task.Dir, Token: "{work}"},
		)
		profile.Result, profile.Reason = "unsupported", reason
	}
	return profile, nil
}

func buildProbeTask(cfg *config.Config, id string, schema []byte, hasSchema bool, dir string, definition processor.Definition) (runner.Task, string, error) {
	input := filepath.Join(dir, "input.json")
	if err := os.WriteFile(input, []byte("{}"), 0o600); err != nil {
		return runner.Task{}, "", fault.Wrap(fault.Internal, err, "write processor probe input")
	}
	schemaPath := filepath.Join(dir, "schema.json")
	if !hasSchema {
		schema = []byte("{}")
	}
	if err := os.WriteFile(schemaPath, schema, 0o600); err != nil {
		return runner.Task{}, "", fault.Wrap(fault.Internal, err, "write processor probe schema")
	}
	schemaInfo := runner.SchemaInfo{ID: id, Path: schemaPath, Ref: "local:" + schemaPath, Origin: "local"}
	inputs := []runner.Input{{Path: input, OriginalPath: input, SchemaID: id}}
	plan, err := runner.BuildPlan(inputs, map[string]runner.SchemaInfo{id: schemaInfo}, runner.Options{Workspace: cfg.Workspace, Spec: definition.Spec, Environ: env.Environ(), Lookup: env.Lookup})
	if err != nil {
		return runner.Task{}, "", fault.Wrap(fault.Usage, err, "plan processor %s", definition.Type)
	}
	if len(plan.Tasks) != 1 {
		return runner.Task{}, "", fault.New(fault.Internal, "processor %s probe plan produced %d tasks", definition.Type, len(plan.Tasks))
	}
	return plan.Tasks[0], schemaPath, nil
}

func lookupPublisherState(previous *state.State, id string) (*state.Schema, bool) {
	if previous == nil {
		return nil, false
	}
	return previous.Lookup(id)
}

func sameProcessorDefinition(a, b processor.Definition) bool {
	a.Spec.Name, b.Spec.Name = "", ""
	return reflect.DeepEqual(a, b)
}

func cachedProfile(previous *state.State, id, schemaDigest, name, tool, settings string) (catalog.Profile, bool) {
	if previous == nil {
		return catalog.Profile{}, false
	}
	rec, ok := previous.Lookup(id)
	if !ok || rec.Entry.Metadata == nil || rec.Entry.Metadata.SchemaDigest != schemaDigest {
		return catalog.Profile{}, false
	}
	for _, profile := range rec.Entry.Metadata.Profiles {
		if profile.Processor == name && profile.Tool == tool && profile.SettingsDigest == settings {
			return profile, true
		}
	}
	return catalog.Profile{}, false
}

func sourceFormats(patterns []string) []catalog.SourceFormat {
	formats := []catalog.SourceFormat{}
	for _, format := range []string{"json", "jsonc", "yaml", "toml", "json5"} {
		ext := map[string]string{"json": ".json", "jsonc": ".jsonc", "yaml": ".yaml", "toml": ".toml", "json5": ".json5"}[format]
		for _, pattern := range patterns {
			lower := strings.ToLower(pattern)
			if strings.Contains(lower, ext) || (format == "yaml" && strings.Contains(lower, ".yml")) {
				extensions := []string{ext}
				if format == "yaml" && strings.Contains(lower, ".yml") {
					extensions = append(extensions, ".yml")
				}
				formats = append(formats, catalog.SourceFormat{Format: format, Extensions: extensions, Filenames: []string{}})
				break
			}
		}
	}
	jsoncNames := []string{"tsconfig.json", "tsconfig.base.json", "tsconfig.build.json", "tsconfig.node.json", "jsconfig.json", ".babelrc", ".babelrc.json", ".cspell.json", ".eslintrc", ".eslintrc.json", ".prettierrc", ".prettierrc.json", ".stylelintrc", ".swcrc", "deno.json", "deno.jsonc", "renovate.json", "settings.json"}
	selected := []string{}
	for _, name := range jsoncNames {
		if matchesSourceName(patterns, name) {
			selected = append(selected, name)
		}
	}
	if len(selected) > 0 {
		formats = slices.DeleteFunc(formats, func(format catalog.SourceFormat) bool { return format.Format == "json" })
		jsonc := -1
		for i := range formats {
			if formats[i].Format == "jsonc" {
				jsonc = i
				break
			}
		}
		if jsonc < 0 {
			formats = append(formats, catalog.SourceFormat{Format: "jsonc", Extensions: []string{}, Filenames: selected})
		} else {
			formats[jsonc].Filenames = append(formats[jsonc].Filenames, selected...)
		}
	}
	slices.SortFunc(formats, func(a, b catalog.SourceFormat) int { return strings.Compare(a.Format, b.Format) })
	return formats
}

func matchesSourceName(patterns []string, name string) bool {
	set, err := match.NewSet(match.OriginCatalog, []match.Rule{{SchemaID: "candidate", Patterns: patterns}})
	if err != nil {
		return false
	}
	return len(set.Match(name)) > 0 || len(set.Match("project/"+name)) > 0
}
