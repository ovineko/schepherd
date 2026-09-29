package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/match"
)

func (a *app) writeJSON(v any) error {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")

	if err := enc.Encode(v); err != nil {
		return fault.Wrap(fault.Internal, err, "encode JSON output")
	}

	return a.writeBytes(buf.Bytes())
}

func (a *app) withCatalog(cmd *cobra.Command, fn func(s *session) error) error {
	ctx, cancel, err := a.withTimeout(cmd.Context())
	if err != nil {
		return err
	}
	defer cancel()

	s, err := a.openSession(ctx)
	if err != nil {
		return err
	}
	defer s.close()

	return fn(s)
}

func (a *app) newCatalogCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Show the verified catalog (--json prints the document byte for byte)",
		Args:  exactArgs(0, "no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withCatalog(cmd, func(s *session) error {
				if asJSON {
					return a.writeBytes(s.catalog.Raw)
				}

				c := s.catalog.Catalog

				return a.write("repository\t" + s.repo.String() + "\ncatalog\t" + s.catalog.Digest + "\nrevision\t" + c.Revision + "\nschemas\t" + strconv.Itoa(len(c.Schemas)) + "\n")
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the catalog document")

	return cmd
}

// withSources runs fn with the schema sources under --timeout.
func (a *app) withSources(cmd *cobra.Command, fn func(ctx context.Context, src *sources) error) error {
	ctx, cancel, err := a.withTimeout(cmd.Context())
	if err != nil {
		return err
	}
	defer cancel()

	src, err := a.openSources(ctx)
	if err != nil {
		return err
	}
	defer src.close()

	return fn(ctx, src)
}

type catalogListItem struct {
	catalog.ListItem

	Origin string `json:"origin"`
}

type localListItem struct {
	ID         string `json:"id"`
	Origin     string `json:"origin"`
	SchemaPath string `json:"schemaPath"`
	Shadows    bool   `json:"shadows"`
}

type catalogPatternItem struct {
	catalog.PatternItem

	Origin string `json:"origin"`
}

type localPatternItem struct {
	ID         string   `json:"id"`
	FileMatch  []string `json:"fileMatch"`
	Origin     string   `json:"origin"`
	SchemaPath string   `json:"schemaPath"`
	Shadows    bool     `json:"shadows"`
}

// listed is one line of list or patterns: the JSON item and its text form.
type listed struct {
	item  any
	id    string
	lines []string
}

// localMarker is the last text column of an entry from [schemas].
func localMarker(shadows bool) string {
	if shadows {
		return originLocal + ", shadows catalog"
	}

	return originLocal
}

// catalogOrEmpty returns the catalog, or an empty one when only local
// schemas are configured.
func (s *sources) catalogOrEmpty(ctx context.Context) (*catalog.Catalog, error) {
	if !s.catalogUsed() {
		return &catalog.Catalog{}, nil
	}

	sess, err := s.session(ctx)
	if err != nil {
		return nil, err
	}

	return sess.catalog.Catalog, nil
}

// shadowedBy reports whether a local schema replaces the catalog entry id.
func (s *sources) shadowedBy(id string) bool {
	_, ok := s.cfg.LocalSchema(id)

	return ok
}

func inCatalog(c *catalog.Catalog, id string) bool {
	_, ok := c.Lookup(id)

	return ok
}

// writeListed prints entries in ID order. A local schema replaces the
// catalog entry with the same ID, so callers never pass both.
func (a *app) writeListed(entries []listed, asJSON bool) error {
	slices.SortStableFunc(entries, func(x, y listed) int { return strings.Compare(x.id, y.id) })

	if asJSON {
		items := make([]any, 0, len(entries))
		for _, e := range entries {
			items = append(items, e.item)
		}

		return a.writeJSON(items)
	}

	var buf bytes.Buffer

	for _, e := range entries {
		for _, line := range e.lines {
			buf.WriteString(line + "\n")
		}
	}

	return a.writeBytes(buf.Bytes())
}

func (a *app) newListCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List schema IDs of the catalog and of the local [schemas]",
		Args:  exactArgs(0, "no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withSources(cmd, func(ctx context.Context, src *sources) error {
				cat, err := src.catalogOrEmpty(ctx)
				if err != nil {
					return err
				}

				var entries []listed

				for _, it := range cat.List() {
					if !src.shadowedBy(it.ID) {
						entries = append(entries, listed{id: it.ID, item: catalogListItem{ListItem: it, Origin: originCatalog}, lines: []string{it.ID + "\t" + it.Name}})
					}
				}

				for _, ls := range src.cfg.LocalSchemas {
					shadows := inCatalog(cat, ls.ID)
					entries = append(entries, listed{
						id:    ls.ID,
						item:  localListItem{ID: ls.ID, Origin: originLocal, SchemaPath: ls.Path, Shadows: shadows},
						lines: []string{ls.ID + "\t" + ls.Path + "\t" + localMarker(shadows)},
					})
				}

				return a.writeListed(entries, asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")

	return cmd
}

func (a *app) newPatternsCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "patterns",
		Short: "Print fileMatch patterns with their schema IDs and artifact descriptors or local files",
		Args:  exactArgs(0, "no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withSources(cmd, func(ctx context.Context, src *sources) error {
				cat, err := src.catalogOrEmpty(ctx)
				if err != nil {
					return err
				}

				var entries []listed

				for _, it := range cat.Patterns() {
					if src.shadowedBy(it.ID) {
						continue
					}

					e := listed{id: it.ID, item: catalogPatternItem{PatternItem: it, Origin: originCatalog}}
					for _, p := range it.FileMatch {
						e.lines = append(e.lines, it.ID+"\t"+p)
					}

					entries = append(entries, e)
				}

				for _, ls := range src.cfg.LocalSchemas {
					if len(ls.FileMatch) == 0 {
						continue
					}

					shadows := inCatalog(cat, ls.ID)
					e := listed{
						id:   ls.ID,
						item: localPatternItem{ID: ls.ID, FileMatch: slices.Clone(ls.FileMatch), Origin: originLocal, SchemaPath: ls.Path, Shadows: shadows},
					}

					for _, p := range ls.FileMatch {
						e.lines = append(e.lines, ls.ID+"\t"+p+"\t"+localMarker(shadows))
					}

					entries = append(entries, e)
				}

				return a.writeListed(entries, asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")

	return cmd
}

// resolution is "resolve --json": Artifact for a catalog schema, SchemaPath
// for a local one.
type resolution struct {
	Artifact   *catalog.Descriptor `json:"artifact,omitempty"`
	File       string              `json:"file"`
	Path       string              `json:"path"`
	Schema     string              `json:"schema"`
	Origin     string              `json:"origin"`
	SchemaPath string              `json:"schemaPath,omitempty"`
}

// resolver matches workspace-relative paths against the levels of
// docs/matching.md: mappings, local schemas' file_match, then catalog
// fileMatch. The catalog is loaded only when the first two levels match
// nothing, and its entries shadowed by local schemas take no part.
type resolver struct {
	src       *sources
	mappings  *match.Set
	local     *match.Set
	catalog   *match.Set
	workspace string
}

func newResolver(src *sources) (*resolver, error) {
	rules := make([]match.Rule, 0, len(src.cfg.Mappings))
	for _, m := range src.cfg.Mappings {
		rules = append(rules, match.Rule{SchemaID: m.Schema, Patterns: m.FileMatch})
	}

	mappings, err := match.NewSet(match.OriginMapping, rules)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "mappings")
	}

	rules = make([]match.Rule, 0, len(src.cfg.LocalSchemas))
	for _, ls := range src.cfg.LocalSchemas {
		if len(ls.FileMatch) > 0 {
			rules = append(rules, match.Rule{SchemaID: ls.ID, Patterns: ls.FileMatch})
		}
	}

	local, err := match.NewSet(match.OriginLocal, rules)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "schemas")
	}

	return &resolver{src: src, mappings: mappings, local: local, workspace: src.cfg.Workspace}, nil
}

func (r *resolver) catalogSet(ctx context.Context) (*match.Set, error) {
	if r.catalog != nil {
		return r.catalog, nil
	}

	cat, err := r.src.catalogOrEmpty(ctx)
	if err != nil {
		return nil, err
	}

	rules := cat.Rules()
	rules = slices.DeleteFunc(rules, func(rule match.Rule) bool { return r.src.shadowedBy(rule.SchemaID) })

	set, err := match.NewSet(match.OriginCatalog, rules)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "catalog fileMatch")
	}

	r.catalog = set

	return set, nil
}

func (r *resolver) resolve(ctx context.Context, file string) (string, match.Resolution, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", match.Resolution{}, fault.Wrap(fault.Usage, err, "path %q", file)
	}

	rel, err := match.RelativePath(r.workspace, abs)
	if err != nil {
		return "", match.Resolution{}, fault.Wrap(fault.NotFound, err, "cannot match %s automatically (workspace %s); pass --schema", abs, r.workspace)
	}

	res, err := match.ResolveFirst(rel, r.mappings, r.local)
	if errors.Is(err, match.ErrNoMatch) {
		set, catalogErr := r.catalogSet(ctx)
		if catalogErr != nil {
			return rel, res, catalogErr
		}

		res, err = match.ResolveFirst(rel, set)
	}

	if err != nil {
		if _, ambiguous := errors.AsType[*match.AmbiguousError](err); ambiguous {
			return rel, res, fault.Wrap(fault.NotFound, err, "ambiguous match; add a [[mappings]] entry or pass --schema")
		}

		return rel, res, fault.Wrap(fault.NotFound, err, "workspace %s (pass --schema or add a [[mappings]] entry)", r.workspace)
	}

	return rel, res, nil
}

func (a *app) newResolveCmd() *cobra.Command {
	var (
		file   string
		asJSON bool
	)

	cmd := &cobra.Command{
		Use:   "resolve --file <path>",
		Short: "Show which schema a path maps to (the file is not read)",
		Args:  exactArgs(0, "no positional arguments (use --file)"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return fault.New(fault.Usage, "--file is required")
			}

			return a.withSources(cmd, func(ctx context.Context, src *sources) error {
				r, err := newResolver(src)
				if err != nil {
					return err
				}

				rel, res, err := r.resolve(ctx, file)
				if err != nil {
					return err
				}

				abs, _ := filepath.Abs(file)
				out := resolution{File: abs, Path: rel, Schema: res.SchemaID, Origin: res.Origin.String()}

				if ls, ok := src.cfg.LocalSchema(res.SchemaID); ok {
					if _, err := src.readLocal(ls); err != nil {
						return err
					}

					out.SchemaPath = ls.Path
				} else {
					_, e, err := src.entry(ctx, res.SchemaID)
					if err != nil {
						return err
					}

					out.Artifact = &e.Artifact
				}

				if !asJSON {
					return a.writeLine("%s", res.SchemaID)
				}

				return a.writeJSON(out)
			})
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "path to resolve (need not exist)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")

	return cmd
}
