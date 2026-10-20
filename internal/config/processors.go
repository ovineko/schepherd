package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/interp"
	"github.com/ovineko/schepherd/internal/match"
	"github.com/ovineko/schepherd/internal/processor"
	"github.com/ovineko/schepherd/internal/runner"
)

func (d *decoder) processorName(n node, m map[string]any) opt[string] {
	return d.checked(n, m, "processor", checkSchemaID)
}

func (d *decoder) processors(root node, doc map[string]any, ly *layer) {
	n, m, ok := d.table(root, doc, "processors")
	if !ok {
		return
	}
	keys := []string{"type", "command", "args", "mode", "cwd", "env", "inherit_env", "output", "formats", "convert"}
	for _, name := range slices.Sorted(maps.Keys(m)) {
		kn, t, ok := d.table(n, m, name)
		if !ok {
			continue
		}
		if err := checkSchemaID(name); err != nil {
			d.failf(kn, "%v", err)
			continue
		}
		d.known(kn, t, keys)
		p := &processorLayer{runner: runnerLayer{env: map[string]opt[string]{}}}
		p.typeName, _ = d.nonEmpty(kn, t, "type")
		if p.typeName.set && !slices.Contains(processor.Types, p.typeName.val) {
			d.failf(d.child(kn, "type"), "unknown processor type %q", p.typeName.val)
		}
		p.output, _ = d.nonEmpty(kn, t, "output")
		if p.output.set && !slices.Contains([]string{"sourcemeta-json", "check-jsonschema-json", "ajv-json", "raw"}, p.output.val) {
			d.failf(d.child(kn, "output"), "unknown output format %q", p.output.val)
		}
		p.formats, _ = d.stringList(kn, t, "formats")
		p.convert, _ = d.templated(kn, t, "convert", pathPlaceholders)
		r := &p.runner
		r.command, _ = d.templated(kn, t, "command", pathPlaceholders)
		r.cwd, _ = d.templated(kn, t, "cwd", pathPlaceholders)
		r.args = d.args(kn, t)
		r.mode, _ = d.nonEmpty(kn, t, "mode")
		if r.mode.set && !slices.Contains(modes, runner.Mode(r.mode.val)) {
			d.failf(d.child(kn, "mode"), "must be batch, per-file or stdin")
		}
		r.inheritEnv, _ = get[bool](d, kn, t, "inherit_env")
		d.runnerEnv(kn, t, r)
		ly.processors[name] = p
	}
}

func (b *builder) processors(cfg *Config) error {
	cfg.Processors = map[string]processor.Definition{}
	cfg.Unsupported = "error"
	if b.merged.runner.unsupported.set {
		cfg.Unsupported = b.merged.runner.unsupported.val
	}
	for _, name := range slices.Sorted(maps.Keys(b.merged.processors)) {
		definition, err := buildProcessorDefinition(name, b.merged.processors[name], cfg.Runner)
		if err != nil {
			return err
		}
		cfg.Processors[name] = definition
	}
	if len(cfg.Processors) > 0 && cfg.Runner.Command != "" {
		return fault.New(fault.Usage, "runner.command and processors are mutually exclusive")
	}
	if len(cfg.Processors) > 0 && (b.merged.runner.args.set || b.merged.runner.mode.set) {
		return fault.New(fault.Usage, "runner.args and runner.mode are only valid with runner.command; configure a custom processor for argument or mode overrides")
	}
	if err := b.processorOrder(cfg); err != nil {
		return err
	}
	for _, schema := range cfg.LocalSchemas {
		if err := cfg.checkProcessorName(schema.Processor); err != nil {
			return err
		}
	}
	for _, mapping := range cfg.Mappings {
		if err := cfg.checkProcessorName(mapping.Processor); err != nil {
			return err
		}
	}
	cfg.RunnerConfigured = cfg.RunnerConfigured || len(cfg.Processors) > 0
	return nil
}

func (b *builder) processorOrder(cfg *Config) error {
	if b.merged.runner.processors.set {
		if len(b.merged.runner.processors.val) == 0 {
			return fault.New(fault.Usage, "runner.processors must not be empty")
		}
		cfg.ProcessorOrder = slices.Clone(b.merged.runner.processors.val)
	} else {
		cfg.ProcessorOrder = slices.Sorted(maps.Keys(cfg.Processors))
		slices.SortStableFunc(cfg.ProcessorOrder, func(a, b string) int {
			return slices.Index(processor.Types, cfg.Processors[a].Type) - slices.Index(processor.Types, cfg.Processors[b].Type)
		})
	}
	seen := map[string]bool{}
	for _, name := range cfg.ProcessorOrder {
		if _, ok := cfg.Processors[name]; !ok {
			return fault.New(fault.Usage, "runner.processors names undeclared processor %q", name)
		}
		if seen[name] {
			return fault.New(fault.Usage, "runner.processors repeats %q", name)
		}
		seen[name] = true
	}
	return nil
}

func buildProcessorDefinition(name string, p *processorLayer, base runner.Spec) (processor.Definition, error) {
	if !p.typeName.set || !p.runner.command.set {
		return processor.Definition{}, fault.New(fault.Usage, "processors.%s requires type and command after merging", name)
	}
	if p.typeName.val == "custom" && !p.runner.args.set {
		return processor.Definition{}, fault.New(fault.Usage, "processors.%s.args is required for custom processors", name)
	}
	if p.typeName.val != "custom" && (p.runner.mode.set || p.runner.args.set) {
		return processor.Definition{}, fault.New(fault.Usage, "processors.%s: typed processors use their fixed per-file command contract; use custom for arbitrary args or mode", name)
	}
	spec := base
	spec.Name = name
	spec.Type = p.typeName.val
	spec.Command = p.runner.command.val
	spec.Mode = runner.ModePerFile
	spec.Args = processor.DefaultArgs(p.typeName.val)
	if p.runner.mode.set {
		spec.Mode = runner.Mode(p.runner.mode.val)
	}
	if p.runner.args.set {
		spec.Args = slices.Clone(p.runner.args.val)
	}
	spec.Env = maps.Clone(base.Env)
	for key, value := range p.runner.env {
		spec.Env[key] = value.val
	}
	if p.runner.cwd.set {
		spec.Cwd = p.runner.cwd.val
		spec.CwdBase = filepath.Dir(p.runner.cwd.at.file)
	}
	if p.runner.inheritEnv.set {
		spec.InheritEnv = p.runner.inheritEnv.val
	}
	output := p.output.val
	if output == "" {
		output = p.typeName.val + "-json"
		if p.typeName.val == "custom" {
			output = "raw"
		}
	}
	spec.Output = output
	definition := processor.Definition{Spec: spec, Type: p.typeName.val, Output: output, Formats: slices.Clone(p.formats.val), Convert: p.convert.val}
	if err := processor.CheckDefinition(definition); err != nil {
		return processor.Definition{}, fault.Wrap(fault.Usage, err, "processors.%s", name)
	}
	return definition, nil
}

func (c *Config) checkProcessorName(name string) error {
	if name != "" {
		if _, ok := c.Processors[name]; !ok {
			return fault.New(fault.Usage, "undeclared processor %q", name)
		}
	}
	return nil
}

// ProcessorFor honors mapping overrides before the local schema's selection.
func (c *Config) ProcessorFor(path, schemaID string) (string, error) {
	rel, err := filepath.Rel(c.Workspace, path)
	if err != nil {
		return "", fault.Wrap(fault.Usage, err, "processor selection")
	}
	chosen := ""
	for _, mapping := range c.Mappings {
		if mapping.Schema != schemaID || mapping.Processor == "" {
			continue
		}
		set, err := match.NewSet(match.OriginMapping, []match.Rule{{SchemaID: schemaID, Patterns: mapping.FileMatch}})
		if err != nil {
			return "", fault.Wrap(fault.Usage, err, "processor mapping")
		}
		if len(set.Match(filepath.ToSlash(rel))) == 0 {
			continue
		}
		if chosen != "" && chosen != mapping.Processor {
			return "", fault.New(fault.NotFound, "ambiguous processor mappings for %s: %s, %s", path, chosen, mapping.Processor)
		}
		chosen = mapping.Processor
	}
	if chosen != "" {
		return chosen, nil
	}
	if schema, ok := c.LocalSchema(schemaID); ok {
		return schema.Processor, nil
	}
	return "", nil
}

func processorFields(name string, p processor.Definition) []runnerField {
	prefix := "processors." + name
	fields := []runnerField{{prefix + ".command", p.Spec.Command}, {prefix + ".cwd", p.Spec.Cwd}}
	for i, arg := range p.Spec.Args {
		fields = append(fields, runnerField{fmt.Sprintf("%s.args[%d]", prefix, i), arg})
	}
	for _, key := range slices.Sorted(maps.Keys(p.Spec.Env)) {
		fields = append(fields, runnerField{prefix + ".env." + key, p.Spec.Env[key]})
	}
	if p.Convert != "" {
		fields = append(fields, runnerField{prefix + ".convert", p.Convert})
	}
	return fields
}

func processorSummary(c *Config) map[string]any {
	out := map[string]any{}
	for name, p := range c.Processors {
		out[name] = map[string]any{"type": p.Type, "command": p.Spec.Command, "args": nonNil(p.Spec.Args), "mode": p.Spec.Mode, "output": p.Output, "formats": nonNil(p.Formats), "convert": p.Convert, "cwd": p.Spec.Cwd, "cwdBase": p.Spec.CwdBase, "env": maps.Clone(p.Spec.Env), "inheritEnv": p.Spec.InheritEnv}
	}
	return out
}

// RunnerUses includes processor and conversion templates when deciding required run resources.
func (c *Config) RunnerUses(placeholder string) bool {
	if c.Runner.Uses(placeholder) {
		return true
	}
	for _, p := range c.Processors {
		if p.Spec.Uses(placeholder) {
			return true
		}
		if tpl, err := interp.Parse(p.Convert); err == nil && tpl.Uses(placeholder) {
			return true
		}
	}
	return false
}
