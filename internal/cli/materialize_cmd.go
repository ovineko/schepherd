package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/materialize"
)

func (a *app) newMaterializeCmd() *cobra.Command {
	var (
		asJSON      bool
		concurrency int
	)

	cmd := &cobra.Command{
		Use:   "materialize",
		Short: "Fill the cache with the whole pinned catalog (verify with --offline)",
		Args:  exactArgs(0, "no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if concurrency < 1 || concurrency > 64 {
				return fault.New(fault.Usage, "--concurrency must be between 1 and 64")
			}

			ctx, cancel, err := a.withTimeout(cmd.Context())
			if err != nil {
				return err
			}
			defer cancel()

			return a.withCatalog(cmd, func(s *session) error {
				result, err := materialize.Run(ctx, s.store, s.catalog, materialize.Options{
					Concurrency: concurrency,
					Log:         a.logf,
				})
				if err != nil {
					return fault.Wrap(fault.Internal, err, "materialize")
				}

				result.Repository = s.repo.String()
				result.CacheDir = s.cache.Dir()

				if !result.Complete {
					for _, m := range result.Missing {
						a.logf("materialize: missing %s %s (%s): %s", m.Kind, m.Digest, m.State, firstIDs(m.IDs))
					}
				}

				a.logf("materialize: %s in %s: fetched %d manifests and %d blobs, %d schema files verified",
					completeWord(result.Complete), result.CacheDir,
					result.FetchedManifests, result.FetchedBlobs,
					result.CachedSchemaFiles)

				if !result.Complete {
					return fault.New(fault.Offline, "the cache is not complete for catalog %s (%d missing entries)", result.CatalogDigest, len(result.Missing))
				}

				if asJSON {
					return a.writeJSON(result)
				}

				return a.write(textSummary(result))
			})
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable summary")
	cmd.Flags().IntVar(&concurrency, "concurrency", materialize.DefaultConcurrency, "schemas verified or fetched in parallel")

	return cmd
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
