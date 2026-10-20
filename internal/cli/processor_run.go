package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/ovineko/schepherd/internal/config"
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

type processorPlanner struct {
	cfg      *config.Config
	schemas  map[string]runner.SchemaInfo
	opts     runner.Options
	compiler *processor.Compiler
	compiled map[processorProbeKey]processor.Prepared
	contents map[string][]byte
	probes   map[processorProbeKey]string
	temp     string
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
	p := processorPlanner{cfg: cfg, schemas: schemas, opts: opts, probes: map[processorProbeKey]string{}, compiler: compiler, compiled: map[processorProbeKey]processor.Prepared{}, contents: map[string][]byte{}}
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
				return nil, skipped, err
			}
			checked[input.SchemaID] = true
		}
		name, actual, reason, err := p.selectInput(ctx, input)
		if err != nil {
			return nil, skipped, err
		}
		if name == "" {
			skipped = append(skipped, runner.Skipped{File: input.Path, Reason: reason})
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
	for _, name := range names {
		definition := p.cfg.Processors[name]
		accepts, convert := definition.Accepts(input.Path)
		if !accepts {
			reasons = append(reasons, fmt.Sprintf("%s does not support input format %q", name, processor.Format(input.Path)))
			continue
		}
		reason, err := p.probe(ctx, name, input, definition)
		if err != nil {
			return "", input, "", err
		}
		if reason != "" {
			reasons = append(reasons, name+": "+reason)
			continue
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
	spec.Command = definition.Convert
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
