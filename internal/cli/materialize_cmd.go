package cli

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/materialize"
)

func (a *app) newMaterializeCmd() *cobra.Command {
	var (
		asJSON             bool
		concurrency        int
		compile            bool
		compilePersistable bool
		compileSchemas     []string
	)

	cmd := &cobra.Command{
		Use:   "materialize",
		Short: "Fill the cache with the whole pinned catalog (verify with --offline)",
		Args:  exactArgs(0, "no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runMaterialize(cmd, materializeFlags{
				asJSON: asJSON, concurrency: concurrency, compile: compile,
				compilePersistable: compilePersistable, compileSchemas: compileSchemas,
			})
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable summary")
	cmd.Flags().IntVar(&concurrency, "concurrency", materialize.DefaultConcurrency, "schemas verified or fetched in parallel")
	cmd.Flags().BoolVar(&compile, "compile", false, "prepare all configured processor compilations strictly (verify only with --offline)")
	cmd.Flags().BoolVar(&compilePersistable, "compile-persistable", false, "prepare only persistable processor compilations; report invocation-local schemas")
	cmd.Flags().StringSliceVar(&compileSchemas, "compile-schema", nil, "compile only these catalog schema IDs (requires --compile or --compile-persistable)")

	return cmd
}

type materializeFlags struct {
	asJSON             bool
	concurrency        int
	compile            bool
	compilePersistable bool
	compileSchemas     []string
}

func (a *app) runMaterialize(cmd *cobra.Command, flags materializeFlags) error {
	if err := validateMaterializeFlags(flags); err != nil {
		return err
	}
	ctx, cancel, err := a.withTimeout(cmd.Context())
	if err != nil {
		return err
	}
	defer cancel()

	s, err := a.openCompletenessSession(ctx)
	if err != nil {
		return err
	}
	defer s.close()

	result, err := materialize.Run(ctx, s.store, s.catalog, materialize.Options{Concurrency: flags.concurrency, Log: a.logf})
	if err != nil {
		return fault.Wrap(fault.Internal, err, "materialize")
	}
	result.Repository, result.CacheDir = s.repo.String(), s.cache.Dir()
	var compilation *compilationSummary
	if (flags.compile || flags.compilePersistable) && result.Complete {
		prepared, err := a.compileCatalog(ctx, s, flags.concurrency, flags.compileSchemas, flags.compilePersistable)
		if err != nil {
			return err
		}
		compilation = &prepared
		result.Complete = prepared.Missing == 0
	}
	if !result.Complete {
		for _, missing := range result.Missing {
			a.logf("materialize: missing %s %s (%s): %s", missing.Kind, missing.Digest, missing.State, firstIDs(missing.IDs))
		}
	}
	a.logf("materialize: %s in %s: fetched %d manifests and %d blobs, %d schema files verified",
		completeWord(result.Complete), result.CacheDir, result.FetchedManifests, result.FetchedBlobs, result.CachedSchemaFiles)
	if err := a.writeMaterializeOutput(flags.asJSON, result, compilation); err != nil {
		return err
	}
	if !result.Complete {
		missing := int64(len(result.Missing))
		if compilation != nil {
			missing += compilation.Missing
		}
		return fault.New(fault.Offline, "the cache is not complete for catalog %s (%d missing entries)", result.CatalogDigest, missing)
	}
	return nil
}

func validateMaterializeFlags(flags materializeFlags) error {
	if flags.concurrency < 1 || flags.concurrency > 64 {
		return fault.New(fault.Usage, "--concurrency must be between 1 and 64")
	}
	if flags.compile && flags.compilePersistable {
		return fault.New(fault.Usage, "--compile and --compile-persistable are mutually exclusive")
	}
	if len(flags.compileSchemas) > 0 && !flags.compile && !flags.compilePersistable {
		return fault.New(fault.Usage, "--compile-schema requires --compile or --compile-persistable")
	}
	return nil
}

func (a *app) writeMaterializeOutput(asJSON bool, result *materialize.Result, compilation *compilationSummary) error {
	if asJSON {
		output := struct {
			*materialize.Result

			Compilation *compilationSummary `json:"compilation,omitempty"`
		}{result, compilation}
		return a.writeJSON(output)
	}
	if err := a.write(textSummary(result)); err != nil {
		return err
	}
	if compilation == nil {
		return nil
	}
	var summary strings.Builder
	fmt.Fprintf(&summary, "compiled\t%d\ncapabilities\t%d\nunsupported\t%d\nmissingCompilations\t%d\n",
		compilation.Artifacts, compilation.Capabilities, compilation.Unsupported, compilation.Missing)
	if compilation.InvocationLocal > 0 {
		fmt.Fprintf(&summary, "invocationLocal\t%d\n", compilation.InvocationLocal)
		for _, reason := range slices.Sorted(maps.Keys(compilation.InvocationLocalReasons)) {
			fmt.Fprintf(&summary, "invocationLocalReason\t%d\t%s\n", compilation.InvocationLocalReasons[reason], reason)
		}
	}
	return a.write(summary.String())
}

func textSummary(r *materialize.Result) string {
	fetched := r.FetchedManifests + r.FetchedBlobs
	cached := r.CachedManifests + r.CachedBlobs

	return "repository\t" + r.Repository + "\n" +
		"catalog\t" + r.CatalogDigest + "\n" +
		"revision\t" + r.Revision + "\n" +
		"cacheDir\t" + r.CacheDir + "\n" +
		"entries\t" + strconv.Itoa(r.Entries) + "\n" +
		"schemas\t" + strconv.Itoa(r.Schemas) + "\n" +
		"notices\t" + strconv.Itoa(r.Notices) + "\n" +
		"fetched\t" + strconv.Itoa(fetched) + "\n" +
		"cached\t" + strconv.Itoa(cached) + "\n"
}

func completeWord(complete bool) string {
	if complete {
		return "complete"
	}

	return "incomplete"
}

func firstIDs(ids []string) string {
	const show = 3

	switch n := len(ids); {
	case n == 0:
		return "no ids"
	case n <= show:
		return fmt.Sprintf("%v", ids)
	default:
		return fmt.Sprintf("%v and %d more", ids[:show], n-show)
	}
}
