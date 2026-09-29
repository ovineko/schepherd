package publish

import (
	"context"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/internal/registry"
)

// Outcomes of Latest for catalog-latest.
const (
	LatestMoved   = "moved"
	LatestCurrent = "current"
	LatestChecked = "checked"
)

// LatestOptions configures Latest.
type LatestOptions struct {
	// Log receives progress messages; nil discards them.
	Log func(format string, args ...any)
	// Repository is reported in the result and in errors.
	Repository string
	// CheckOnly verifies the recorded catalog and leaves catalog-latest
	// alone.
	CheckOnly bool
}

// LatestResult is the machine-readable outcome of Latest. Latest is
// LatestMoved, LatestCurrent (it already pointed there) or LatestChecked.
type LatestResult struct {
	Repository    string `json:"repository"`
	Revision      string `json:"revision"`
	CatalogDigest string `json:"catalogDigest"`
	Latest        string `json:"latest"`
	CatalogSize   int64  `json:"catalogSize"`
}

// Latest points catalog-latest at the catalog st records. It needs no
// prepared set, so a publication whose state is recorded in Git can be
// finished however much upstream changed since. First it verifies that
// catalog-<revision> names exactly the recorded index and that this
// catalog lists exactly the recorded entries; a mismatch is fault.Integrity
// and changes nothing.
func Latest(ctx context.Context, target Target, st *state.State, opts LatestOptions) (*LatestResult, error) {
	if st == nil {
		return nil, fault.New(fault.Usage, "no state: catalog-latest can only point at a recorded catalog")
	}

	if err := st.Validate(); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "state")
	}

	r := &run{target: target, opts: Options{State: st, Repository: opts.Repository}, log: opts.Log}
	if r.log == nil {
		r.log = func(string, ...any) {}
	}

	desc, err := r.recordedTag(ctx)
	if err != nil {
		return nil, err
	}

	if err := r.checkRecordedEntries(ctx); err != nil {
		return nil, err
	}

	res := &LatestResult{
		Repository: opts.Repository, Revision: st.Catalog.Revision, CatalogDigest: st.Catalog.Digest,
		CatalogSize: st.Catalog.Size, Latest: LatestChecked,
	}

	if opts.CheckOnly {
		r.log("%s names the recorded catalog %s", calver.TagPrefix+st.Catalog.Revision, st.Catalog.Digest)

		return res, nil
	}

	if err := r.moveLatest(ctx, desc); err != nil {
		return nil, err
	}

	res.Latest = LatestCurrent
	if len(r.created) > 0 {
		res.Latest = LatestMoved
	}

	r.log("%s points to revision %s (%s)", LatestTag, st.Catalog.Revision, st.Catalog.Digest)

	return res, nil
}

// recordedTag returns the index of the catalog the state records after
// checking that its revision tag names exactly that index.
func (r *run) recordedTag(ctx context.Context) (ocispec.Descriptor, error) {
	current := r.opts.State.Catalog
	tag := calver.TagPrefix + current.Revision

	desc, err := r.target.Resolve(ctx, tag)

	switch {
	case registry.IsNotFound(err):
		return ocispec.Descriptor{}, fault.New(fault.Integrity, "the state records catalog %s as revision %s, but %s does not exist in %s",
			current.Digest, current.Revision, tag, r.opts.Repository)
	case err != nil:
		return ocispec.Descriptor{}, fault.Wrap(fault.Registry, err, "resolve %s", tag)
	case desc.Digest.String() != current.Digest || desc.Size != current.Size:
		return ocispec.Descriptor{}, fault.New(fault.Integrity, "the state records catalog %s (%d bytes) as revision %s, but %s points to %s (%d bytes)",
			current.Digest, current.Size, current.Revision, tag, desc.Digest, desc.Size)
	}

	return desc, nil
}

// checkRecordedEntries fetches the catalog the state records and checks
// that it holds the recorded revision and exactly the recorded entries, so
// a state edited by hand never becomes the record of a published catalog.
func (r *run) checkRecordedEntries(ctx context.Context) error {
	current := r.opts.State.Catalog

	published, err := fetchCatalog(ctx, r.target, current.Digest)
	if err != nil {
		return err
	}

	recorded := &catalog.Catalog{FormatVersion: catalog.FormatVersion, Revision: current.Revision, Schemas: r.opts.State.Entries()}
	if published.Revision != current.Revision || !sameContent(recorded, published) {
		return fault.New(fault.Integrity, "catalog %s (%s) does not list the entries the state records for revision %s",
			current.Digest, calver.TagPrefix+current.Revision, current.Revision)
	}

	return nil
}
