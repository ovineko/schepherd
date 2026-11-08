package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/ovineko/schepherd/internal/config"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/processor"
	"github.com/ovineko/schepherd/internal/runner"
)

type (
	processorProbeKey  struct{ schema, processor, file string }
	processorSelection struct {
		name   string
		inputs []runner.Input
	}
)

type processorIdentity struct {
	tool     string
	settings string
}

type processorPlanner struct {
	cfg        *config.Config
	schemas    map[string]runner.SchemaInfo
	opts       runner.Options
	compiler   *processor.Compiler
	compiled   map[processorProbeKey]processor.Prepared
	contents   map[string][]byte
	probes     map[processorProbeKey]string
	identities map[string]processorIdentity
	temp       string
}

func buildCompiledProcessorPlan(ctx context.Context, cfg *config.Config, inputs []runner.Input, schemas map[string]runner.SchemaInfo, opts runner.Options, compiler *processor.Compiler) (*runner.Plan, []runner.Skipped, func(), error) {
	if len(cfg.Processors) == 0 {
		opts.Spec.Name = "runner"
		opts.Spec.Type = "custom"
		opts.Spec.Output = "raw"
		plan, err := runner.BuildPlan(inputs, schemas, opts)
		if err != nil {
			return nil, nil, nil, fault.Wrap(fault.Usage, err, "runner plan")
		}
		return plan, nil, nil, nil
	}
	p := processorPlanner{cfg: cfg, schemas: schemas, opts: opts, probes: map[processorProbeKey]string{}, compiler: compiler, compiled: map[processorProbeKey]processor.Prepared{}, contents: map[string][]byte{}, identities: map[string]processorIdentity{}}
	groups, skipped, err := p.selectInputs(ctx, inputs)
	if err != nil {
		p.cleanup()
		return nil, skipped, nil, err
	}
	plan, err := p.buildGroups(groups)
	if err != nil {
		p.cleanup()
		return nil, skipped, nil, err
	}
	return plan, skipped, p.cleanup, nil
}

func (p *processorPlanner) cleanup() {
	p.compiler.Close()
	if p.temp != "" {
		_ = os.RemoveAll(p.temp)
	}
}

func (p *processorPlanner) checkSchema(id string) error {
	schema, ok := p.schemas[id]
	if !ok {
		return fault.New(fault.Internal, "processor schema %s was not prepared", id)
	}
	limit := p.cfg.Limits.MaxSchemaBytes
	if limit == 0 {
		limit = config.DefaultLimits().MaxSchemaBytes
	}
	local, ok := p.cfg.LocalSchema(id)
	if !ok {
		local = &config.LocalSchema{ID: id, Path: schema.Path}
	}
	data, err := local.Read(limit)
	if err != nil {
		return fault.Wrap(fault.Usage, err, "processor schema")
	}
	p.contents[id] = data
	return nil
}

func withProcessorInput(err error, path string) error {
	if existing, ok := errors.AsType[*runFilesError](err); ok && (existing.file != "" || len(existing.files) > 0) {
		return err
	}
	return &runFilesError{err: err, file: path, files: []string{path}}
}

func (p *processorPlanner) selectInputs(ctx context.Context, inputs []runner.Input) ([]processorSelection, []runner.Skipped, error) {
	groups := []processorSelection{}
	skipped := []runner.Skipped{}
	groupIndex := map[string]int{}
	seen := map[string]bool{}
	checked := map[string]bool{}
	for _, input := range inputs {
		if seen[input.Path] {
			continue
		}
		seen[input.Path] = true
		if !checked[input.SchemaID] {
			if err := p.checkSchema(input.SchemaID); err != nil {
				return nil, skipped, withProcessorInput(err, input.Path)
			}
			checked[input.SchemaID] = true
		}
		name, actual, reason, err := p.selectInput(ctx, input)
		if err != nil {
			return nil, skipped, withProcessorInput(err, input.Path)
		}
		if name == "" {
			skipped = append(skipped, runner.Skipped{File: input.Path, Reason: reason, Category: runner.SkipUnsupportedProcessor})
			continue
		}
		index, exists := groupIndex[name]
		if !exists {
			index = len(groups)
			groupIndex[name] = index
			groups = append(groups, processorSelection{name: name})
		}
		groups[index].inputs = append(groups[index].inputs, actual)
	}
	return groups, skipped, nil
}

func (p *processorPlanner) selectInput(ctx context.Context, input runner.Input) (string, runner.Input, string, error) {
	explicit, err := p.cfg.ProcessorFor(input.Path, input.SchemaID)
	if err != nil {
		return "", input, "", fault.Wrap(fault.Usage, err, "processor selection")
	}
	names := p.cfg.ProcessorOrder
	if explicit != "" {
		names = []string{explicit}
	}
	reasons := []string{}
	multiDocumentYAML := false
	sourceFormat := processor.FormatFor(input.Path, p.schemas[input.SchemaID].SourceFormats)
	input.Format = sourceFormat
	if sourceFormat == "yaml" {
		count, err := processor.YAMLDocumentCount(input.Path)
		if err != nil {
			return "", input, "", fault.Wrap(fault.Usage, err, "parse YAML input %s", input.Path)
		}
		multiDocumentYAML = count > 1
	}
	for _, name := range names {
		definition := p.cfg.Processors[name]
		format := sourceFormat
		if multiDocumentYAML && definition.Type == "sourcemeta" {
			reasons = append(reasons, name+" does not support multi-document YAML")
			continue
		}
		accepts, convert := definition.AcceptsFormat(format)
		if !accepts {
			reasons = append(reasons, fmt.Sprintf("%s does not support input format %q", name, format))
			continue
		}
		reason, trusted, err := p.profile(ctx, name, input, definition)
		if err != nil {
			return "", input, "", err
		}
		if trusted && reason != "" {
			reasons = append(reasons, name+": "+reason)
			continue
		}
		if trusted && definition.Type == "check-jsonschema" {
			key := probeKey(input.SchemaID, name, input.Path, definition)
			p.probes[key] = ""
			p.compiled[key] = processor.Prepared{}
		} else {
			reason, err = p.probe(ctx, name, input, definition)
			if err != nil {
				return "", input, "", err
			}
			if reason != "" {
				reasons = append(reasons, name+": "+reason)
				continue
			}
		}
		if convert {
			input, err = p.convert(ctx, input, definition)
			if err != nil {
				return "", input, "", err
			}
		}
		return name, input, "", nil
	}
	err = processor.Unsupported(input.Path, reasons)
	if p.cfg.Unsupported != "skip" {
		return "", input, "", fault.Wrap(fault.Usage, err, "processor selection")
	}
	return "", input, err.Error(), nil
}

func (p *processorPlanner) profile(ctx context.Context, name string, input runner.Input, definition processor.Definition) (string, bool, error) {
	if processor.InputDependent(definition) {
		return "", false, nil
	}
	schema := p.schemas[input.SchemaID]
	if schema.Origin != originCatalog || schema.SchemaDigest == "" || digest.FromBytes(p.contents[input.SchemaID]) != schema.SchemaDigest {
		return "", false, nil
	}
	candidates := 0
	for _, profile := range schema.ProcessorProfiles {
		if profile.Processor == definition.Type {
			candidates++
		}
	}
	if candidates == 0 {
		return "", false, nil
	}
	identity, ok := p.identities[name]
	if !ok {
		plan, err := p.plan([]runner.Input{input}, definition.Spec)
		if err != nil {
			return "", false, err
		}
		task := plan.Tasks[0]
		tool, settings, err := processor.Identity(ctx, definition, task)
		if err != nil {
			return "", false, fault.Wrap(fault.ConsumerStart, err, "processor %s identity", name)
		}
		identity = processorIdentity{tool: tool, settings: settings}
		p.identities[name] = identity
	}
	var matched *runner.ProcessorProfile
	for i := range schema.ProcessorProfiles {
		profile := &schema.ProcessorProfiles[i]
		if profile.Processor == definition.Type && profile.Tool == identity.tool && profile.SettingsDigest == identity.settings {
			if matched != nil {
				return "", false, fault.New(fault.Integrity, "ambiguous processor profiles for schema %s and processor %s", input.SchemaID, definition.Type)
			}
			matched = profile
		}
	}
	if matched == nil {
		return "", false, nil
	}
	if matched.Result == "unsupported" {
		return matched.Reason, true, nil
	}
	return "", matched.Result == "supported", nil
}

func (p *processorPlanner) plan(inputs []runner.Input, spec runner.Spec) (*runner.Plan, error) {
	opts := p.opts
	opts.Spec = spec
	plan, err := runner.BuildPlan(inputs, p.schemas, opts)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "processor plan")
	}
	return plan, nil
}

func (p *processorPlanner) probe(ctx context.Context, name string, input runner.Input, definition processor.Definition) (string, error) {
	key := probeKey(input.SchemaID, name, input.Path, definition)
	if reason, done := p.probes[key]; done {
		return reason, nil
	}
	candidate, err := p.plan([]runner.Input{input}, definition.Spec)
	if err != nil {
		return "", err
	}
	prepared, err := p.compiler.Prepare(ctx, definition, candidate.Tasks[0], p.schemas[input.SchemaID].Path, p.contents[input.SchemaID], p.schemas[input.SchemaID].Origin == originCatalog)
	if err != nil {
		return "", fault.Wrap(fault.ConsumerStart, err, "processor capability")
	}
	p.compiled[key] = prepared
	p.probes[key] = prepared.Reason
	return prepared.Reason, nil
}

func (p *processorPlanner) convert(ctx context.Context, input runner.Input, definition processor.Definition) (runner.Input, error) {
	if p.temp == "" {
		directory, err := os.MkdirTemp("", "schepherd-inputs-*")
		if err != nil {
			return input, fault.Wrap(fault.Internal, err, "create conversion directory")
		}
		p.temp = directory
	}
	spec := definition.Spec
	spec.Mode = runner.ModePerFile
	spec.Command = definition.Convert
	spec.Args = slices.Clone(definition.ConvertArgs)
	converter, err := p.plan([]runner.Input{input}, spec)
	if err != nil {
		return input, err
	}
	task := converter.Tasks[0]
	converted, err := processor.Convert(ctx, task.Path, task, input.Path, p.temp)
	if err != nil {
		return input, fault.Wrap(fault.ConsumerStart, err, "processor conversion")
	}
	input.OriginalPath = input.Path
	input.Path = converted
	input.Format = "json"
	return input, nil
}

func (p *processorPlanner) buildGroups(groups []processorSelection) (*runner.Plan, error) {
	plan := &runner.Plan{Mode: runner.ModePerFile, Tasks: []runner.Task{}}
	for _, group := range groups {
		selected, err := p.plan(group.inputs, p.cfg.Processors[group.name].Spec)
		if err != nil {
			return nil, err
		}
		if len(plan.Tasks) > 0 && plan.Mode != selected.Mode {
			return nil, fault.New(fault.Usage, "selected processors use different runner modes; use per-file mode")
		}
		for i := range selected.Tasks {
			task := &selected.Tasks[i]
			if task.Processor.Type == "check-jsonschema" && len(task.Files) > 0 {
				format := processor.FormatFor(task.Files[0], p.schemas[task.SchemaID].SourceFormats)
				if len(task.Paths) > 0 && task.Paths[0] != task.Files[0] {
					format = "json"
				}
				task.Args = append(task.Args, "--format", format)
			}
			key := probeKey(task.SchemaID, group.name, task.Files[0], p.cfg.Processors[group.name])
			p.compiled[key].Apply(task)
		}
		plan.Mode = selected.Mode
		plan.Tasks = append(plan.Tasks, selected.Tasks...)
	}
	for i := range plan.Tasks {
		plan.Tasks[i].Index = i
	}
	return plan, nil
}

func probeKey(schema, name, file string, definition processor.Definition) processorProbeKey {
	if !processor.InputDependent(definition) {
		file = ""
	}
	return processorProbeKey{schema: schema, processor: name, file: file}
}
