package cli

import (
	"context"
	"path/filepath"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/config"
	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/interp"
	"github.com/ovineko/schepherd/internal/match"
	"github.com/ovineko/schepherd/internal/runner"
)

// Values of "origin" for where a schema comes from, in list, patterns and
// run reports.
const (
	originLocal   = "local"
	originCatalog = "catalog"
)

// sources hands out schemas by ID: local [schemas] entries straight from
// their files, catalog entries through a session. The session is opened only
// when a catalog schema or catalog fileMatch is actually needed, so a
// command that uses only local schemas never contacts a registry and never
// opens the cache.
type sources struct {
	app  *app
	cfg  *config.Config
	sess *session
}

func (a *app) openSources(ctx context.Context) (*sources, error) {
	cfg, err := a.config()
	if err != nil {
		return nil, err
	}

	s := &sources{app: a, cfg: cfg}

	// Without local schemas every schema comes from the catalog, so it is
	// loaded first and its problems are reported before those of any input,
	// exactly as for a configuration that predates [schemas].
	if len(cfg.LocalSchemas) == 0 {
		if _, err := s.session(ctx); err != nil {
			return nil, err
		}
	}

	return s, nil
}

func (s *sources) close() {
	if s.sess != nil {
		s.sess.close()
	}
}

// catalogUsed reports whether the catalog takes part. Without local schemas
// it always does, so a configuration that names no schema source at all
// keeps failing with the missing-catalog error instead of matching nothing.
func (s *sources) catalogUsed() bool {
	return s.cfg.Repository != "" || s.cfg.Catalog != "" || len(s.cfg.LocalSchemas) == 0
}

func (s *sources) session(ctx context.Context) (*session, error) {
	if s.sess != nil {
		return s.sess, nil
	}

	sess, err := s.app.openSession(ctx)
	if err != nil {
		return nil, err
	}

	s.sess = sess

	return sess, nil
}

// entry looks up an ID that is not a local schema in the catalog.
func (s *sources) entry(ctx context.Context, id string) (*session, *catalog.Entry, error) {
	if !s.catalogUsed() {
		return nil, nil, fault.New(fault.Usage,
			"schema %q is not declared in [schemas] and no catalog is configured; set catalog.repository and catalog.digest (or %s and %s) to use catalog schemas",
			id, env.KeyRepository, env.KeyCatalog)
	}

	sess, err := s.session(ctx)
	if err != nil {
		return nil, nil, err
	}

	e, err := sess.entry(id)
	if err != nil {
		return nil, nil, err
	}

	return sess, e, nil
}

// known fails unless id is a local schema or a catalog entry.
func (s *sources) known(ctx context.Context, id string) error {
	if _, ok := s.cfg.LocalSchema(id); ok {
		return nil
	}

	_, _, err := s.entry(ctx, id)

	return err
}

// preparedSchema is a schema ready for use: info describes the file a
// consumer reads, content returns its bytes and notice the attribution and
// license text published with it (nil when there is none).
type preparedSchema struct {
	content func() ([]byte, error)
	notice  func() ([]byte, error)
	info    runner.SchemaInfo
}

// prepare checks a local schema file in place or materializes a catalog
// schema into the cache. A local schema is never copied, so a consumer sees
// edits at once and relative $refs to its sibling files keep working.
func (s *sources) prepare(ctx context.Context, id string) (preparedSchema, error) {
	if ls, ok := s.cfg.LocalSchema(id); ok {
		data, err := s.readLocal(ls)
		if err != nil {
			return preparedSchema{}, err
		}

		return preparedSchema{
			info:    runner.SchemaInfo{ID: id, Path: ls.Path, Ref: localRef(s.cfg.Workspace, ls.Path), Origin: originLocal},
			content: func() ([]byte, error) { return data, nil },
			notice:  func() ([]byte, error) { return nil, nil },
		}, nil
	}

	sess, e, err := s.entry(ctx, id)
	if err != nil {
		return preparedSchema{}, err
	}

	sch, err := sess.store.Materialize(ctx, e)
	if err != nil {
		return preparedSchema{}, fault.Wrap(fault.Internal, err, "schema %q", id)
	}

	return preparedSchema{
		info: runner.SchemaInfo{ID: sch.ID, Path: sch.Path, Ref: sch.Ref, Origin: originCatalog},
		content: func() ([]byte, error) {
			data, err := sess.store.ReadSchema(sch)
			if err != nil {
				return nil, fault.Wrap(fault.Integrity, err, "schema %q", sch.ID)
			}

			return data, nil
		},
		notice: func() ([]byte, error) {
			text, err := sess.store.Notice(ctx, sch)
			if err != nil {
				return nil, fault.Wrap(fault.Internal, err, "schema %q", sch.ID)
			}

			return text, nil
		},
	}, nil
}

// readLocal checks a local schema file the way every command that uses it
// must, and returns its content.
func (s *sources) readLocal(ls *config.LocalSchema) ([]byte, error) {
	data, err := ls.Read(s.cfg.Limits.MaxSchemaBytes)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "local schema %q", ls.ID)
	}

	return data, nil
}

// cacheDir is the value of {cache}. Without a session it is computed from
// the configuration only, so a run on local schemas never creates the cache,
// and it is empty when no directory can be determined (no --cache-dir and no
// home directory) but the runner never expands {cache}.
func (s *sources) cacheDir() (string, error) {
	if s.sess != nil {
		return s.sess.cache.Dir(), nil
	}

	dir, err := cacheDirPath(s.cfg)
	if err != nil && !s.cfg.Runner.Uses(interp.Cache) {
		return "", nil
	}

	return dir, err
}

// localRef is {schema-ref} of a local schema: "local:" and the path relative
// to the workspace with "/" separators, or the absolute path when the file
// lies outside the workspace.
func localRef(workspace, path string) string {
	if rel, err := match.RelativePath(workspace, path); err == nil {
		return "local:" + rel
	}

	return "local:" + filepath.Clean(path)
}
