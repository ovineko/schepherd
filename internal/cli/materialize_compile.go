package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/ovineko/schepherd/internal/catalog"
	"golang.org/x/sync/errgroup"

	"github.com/ovineko/schepherd/internal/config"
	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/processor"
	"github.com/ovineko/schepherd/internal/runner"
)

type compilationSummary struct {
	Artifacts              int64            `json:"artifacts"`
	Capabilities           int64            `json:"capabilities"`
	Unsupported            int64            `json:"unsupported"`
	Missing                int64            `json:"missing"`
	InvocationLocal        int64            `json:"invocationLocal"`
	InvocationLocalReasons map[string]int64 `json:"invocationLocalReasons,omitempty"`
}

type compilationProgress struct {
	artifacts       atomic.Int64
	capabilities    atomic.Int64
	unsupported     atomic.Int64
	missing         atomic.Int64
	invocationLocal atomic.Int64
	reasonMu        sync.Mutex
	reasons         map[string]int64
}

func (p *compilationProgress) invocationLocalReason(processor, reason string) {
	p.invocationLocal.Add(1)
	p.reasonMu.Lock()
	defer p.reasonMu.Unlock()
	if p.reasons == nil {
		p.reasons = map[string]int64{}
	}
	p.reasons[fmt.Sprintf("%s: %s", processor, reason)]++
}

func (p *compilationProgress) summary() compilationSummary {
	return compilationSummary{
		Artifacts: p.artifacts.Load(), Capabilities: p.capabilities.Load(),
		Unsupported: p.unsupported.Load(), Missing: p.missing.Load(),
		InvocationLocal: p.invocationLocal.Load(), InvocationLocalReasons: p.reasons,
	}
}

func (a *app) compileCatalog(ctx context.Context, s *session, concurrency int, only []string, allowInvocationLocal bool) (compilationSummary, error) {
	if len(s.cfg.Processors) == 0 {
		mode := "--compile"
		if allowInvocationLocal {
			mode = "--compile-persistable"
		}
		return compilationSummary{}, fault.New(fault.Usage, "materialize %s requires configured processors", mode)
	}
	if err := s.cfg.CheckRunnerEnv(env.Lookup); err != nil {
		return compilationSummary{}, fault.Wrap(fault.Usage, err, "compilation processors")
	}
	entries, err := compilationEntries(s, only)
	if err != nil {
		return compilationSummary{}, err
	}
	progress := &compilationProgress{}
	group, ctx := errgroup.WithContext(ctx)
	for worker := range concurrency {
		group.Go(func() error {
			compiler := &processor.Compiler{Cache: s.cache, VerifyOnly: s.cfg.Offline, RequirePersistent: true}
			defer compiler.Close()
			return a.compileCatalogWorker(ctx, s, compiler, entries, worker, concurrency, allowInvocationLocal, progress)
		})
	}
	if err := group.Wait(); err != nil {
		return compilationSummary{}, fault.Wrap(fault.Internal, err, "catalog compilation")
	}
	return progress.summary(), nil
}

func (a *app) compileCatalogWorker(ctx context.Context, s *session, compiler *processor.Compiler, entries []catalog.Entry, worker, concurrency int, allowInvocationLocal bool, progress *compilationProgress) error {
	for index := worker; index < len(entries); index += concurrency {
		if err := ctx.Err(); err != nil {
			return fault.Wrap(fault.Canceled, err, "catalog compilation")
		}
		if err := a.compileCatalogEntry(ctx, s, compiler, &entries[index], allowInvocationLocal, progress); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) compileCatalogEntry(ctx context.Context, s *session, compiler *processor.Compiler, entry *catalog.Entry, allowInvocationLocal bool, progress *compilationProgress) error {
	schema, err := s.store.Materialize(ctx, entry)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "prepare schema %s for compilation", entry.ID)
	}
	data, err := s.store.ReadSchema(schema)
	if err != nil {
		return fault.Wrap(fault.Integrity, err, "read schema %s for compilation", entry.ID)
	}
	for _, name := range s.cfg.ProcessorOrder {
		definition := s.cfg.Processors[name]
		if definition.Type == "custom" {
			continue
		}
		if allowInvocationLocal {
			reason, err := processor.Persistability(definition, data, true)
			if err != nil {
				return fault.Wrap(fault.Integrity, err, "classify schema %s persistence with %s", entry.ID, name)
			}
			if reason != "" {
				progress.invocationLocalReason(definition.Type, reason)
				continue
			}
		}
		task, err := compilationTask(s.cfg, definition, schema.ID, schema.Path, schema.Ref, s.cache.Dir())
		if err != nil {
			return err
		}
		prepared, err := compiler.Prepare(ctx, definition, task, schema.Path, data, true)
		if err != nil {
			if s.cfg.Offline && fault.KindOf(err) == fault.Offline {
				progress.missing.Add(1)
				continue
			}
			return fault.Wrap(fault.Internal, err, "compile schema %s with %s", entry.ID, name)
		}
		switch {
		case prepared.Reason != "":
			progress.unsupported.Add(1)
		case prepared.Path != "":
			progress.artifacts.Add(1)
		default:
			progress.capabilities.Add(1)
		}
	}
	return nil
}

func compilationEntries(s *session, only []string) ([]catalog.Entry, error) {
	if len(only) == 0 {
		return s.catalog.Catalog.Schemas, nil
	}
	entries := make([]catalog.Entry, 0, len(only))
	seen := map[string]bool{}
	for _, id := range only {
		if seen[id] {
			continue
		}
		entry, err := s.entry(id)
		if err != nil {
			return nil, err
		}
		seen[id] = true
		entries = append(entries, *entry)
	}
	return entries, nil
}

func compilationTask(cfg *config.Config, definition processor.Definition, id, path, ref, cacheDir string) (runner.Task, error) {
	// Compilation has no input document. Refuse settings that would depend on one.
	if placeholder := processor.InputDependentPlaceholder(definition); placeholder != "" {
		return runner.Task{}, fault.New(fault.Usage, "processor %s uses {%s} in compilation settings", definition.Spec.Name, placeholder)
	}
	spec := definition.Spec
	spec.Mode, spec.Args = runner.ModeStdin, nil
	plan, err := runner.BuildPlan([]runner.Input{{Path: path, SchemaID: id}}, map[string]runner.SchemaInfo{
		id: {ID: id, Path: path, Ref: ref, Origin: originCatalog},
	}, runner.Options{Spec: spec, Workspace: cfg.Workspace, CacheDir: cacheDir, Lookup: env.Lookup, Environ: env.Environ()})
	if err != nil {
		return runner.Task{}, fault.Wrap(fault.Usage, err, "compilation processor")
	}
	if len(plan.Tasks) != 1 || !filepath.IsAbs(plan.Tasks[0].Path) {
		return runner.Task{}, fault.Wrap(fault.Internal, errors.New("expected one resolved compiler task"), "compilation processor")
	}
	return plan.Tasks[0], nil
}
