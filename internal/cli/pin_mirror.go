package cli

import (
	"context"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/config"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/mirror"
	"github.com/ovineko/schepherd/internal/registry"
	"github.com/ovineko/schepherd/internal/store"
)

type pinResult struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag,omitempty"`
	Digest     string `json:"digest"`
	Revision   string `json:"revision"`
	Schemas    int    `json:"schemas"`
}

func (a *app) newPinCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "pin <repository>:<tag>|<repository>@<digest>",
		Short: "Resolve a catalog tag to a digest once (explicit network operation)",
		Args:  exactArgs(1, "one reference"),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := registry.ParseReference(args[0])
			if err != nil {
				return fault.Wrap(fault.Usage, err, "pin")
			}

			cfg, err := a.transportConfig()
			if err != nil {
				return err
			}

			ctx, cancel, err := a.withTimeout(cmd.Context())
			if err != nil {
				return err
			}
			defer cancel()

			result, err := pin(ctx, cfg, ref)
			if err != nil {
				return err
			}

			if asJSON {
				return a.writeJSON(result)
			}

			return a.write("[catalog]\nrepository = " + strconv.Quote(result.Repository) + "\ndigest = " + strconv.Quote(result.Digest) + "  # revision " + result.Revision + "\n")
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")

	return cmd
}

func pin(ctx context.Context, cfg *config.Config, ref registry.Reference) (*pinResult, error) {
	repo, err := registryClient(cfg).Open(ref.Repository)
	if err != nil {
		return nil, fault.Wrap(fault.Registry, err, "open %s", ref.Repository)
	}

	dgst := ref.Digest
	if ref.Tag != "" {
		desc, err := repo.Resolve(ctx, ref.Tag)
		if err != nil {
			return nil, fault.Wrap(fault.Registry, err, "resolve %s", ref)
		}

		dgst = desc.Digest.String()
	}

	snap, err := store.FetchCatalog(ctx, repo, dgst, artifactLimits(cfg), catalogLimits(cfg))
	if err != nil {
		if fault.KindOf(err) == fault.Integrity {
			return nil, fault.Wrap(fault.Integrity, err, "%s does not point to a valid Schepherd catalog", ref)
		}

		return nil, fault.Wrap(fault.Registry, err, "pin %s", ref)
	}

	c := snap.Catalog

	return &pinResult{Repository: ref.Repository.String(), Tag: ref.Tag, Digest: dgst, Revision: c.Revision, Schemas: len(c.Schemas)}, nil
}

func (a *app) newMirrorCmd() *cobra.Command {
	var (
		asJSON      bool
		concurrency int
	)

	cmd := &cobra.Command{
		Use:   "mirror <source-repository>@<catalog-digest> <destination-repository>",
		Short: "Copy a complete catalog snapshot to another repository, byte for byte",
		Args:  exactArgs(2, "a source reference and a destination repository"),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, err := registry.ParseReference(args[0])
			if err != nil {
				return fault.Wrap(fault.Usage, err, "mirror source")
			}

			if src.Digest == "" {
				return fault.New(fault.Usage, "the source must be pinned by digest (<repository>@sha256:...); use 'schepherd pin' to resolve a tag")
			}

			dstRepo, err := registry.ParseRepository(args[1])
			if err != nil {
				return fault.Wrap(fault.Usage, err, "mirror destination")
			}

			cfg, err := a.transportConfig()
			if err != nil {
				return err
			}

			if concurrency < 1 || concurrency > 64 {
				return fault.New(fault.Usage, "--concurrency must be between 1 and 64")
			}

			ctx, cancel, err := a.withTimeout(cmd.Context())
			if err != nil {
				return err
			}
			defer cancel()

			client := registryClient(cfg)

			srcRepo, err := client.Open(src.Repository)
			if err != nil {
				return fault.Wrap(fault.Registry, err, "open %s", src.Repository)
			}

			dst, err := client.Open(dstRepo)
			if err != nil {
				return fault.Wrap(fault.Registry, err, "open %s", dstRepo)
			}

			result, err := mirror.Run(ctx, srcRepo, dst, src.Digest, mirror.Options{
				ArtifactLimits: artifactLimits(cfg),
				CatalogLimits:  catalogLimits(cfg),
				Concurrency:    concurrency,
				Log:            a.logf,
			})
			if err != nil {
				return fault.Wrap(fault.Internal, err, "mirror")
			}

			if asJSON {
				return a.writeJSON(result)
			}

			return a.write(result.Destination + "@" + result.CatalogDigest + "\n")
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable summary")
	cmd.Flags().IntVar(&concurrency, "concurrency", mirror.DefaultConcurrency, "manifests and blobs copied in parallel")

	return cmd
}
