package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

// fakeRepo is an in-memory Target with the semantics of registry.Repo: it
// verifies pushed content against descriptors, accepts a manifest only when
// its blobs are present and an index only when its manifests are, never
// moves a tag through EnsureTag and reports missing tags with
// errdef.ErrNotFound.
type fakeRepo struct {
	blobs     map[string][]byte
	manifests map[string][]byte
	// mediaTypes holds the media type each manifest was pushed with.
	mediaTypes map[string]string
	tags       map[string]string
	// hidden tags resolve but are not listed, like a tag another publisher
	// creates between listing and tagging.
	hidden map[string]bool
	// failTag makes Tag and EnsureTag fail once for the tag.
	failTag map[string]bool
	// failPush makes PushBlob or PushManifest fail once for the digest.
	failPush map[string]bool
	calls    map[string]int
	stored   []string
	mu       sync.Mutex
}

var errInjected = errors.New("injected failure")

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		blobs: map[string][]byte{}, manifests: map[string][]byte{}, mediaTypes: map[string]string{}, tags: map[string]string{},
		hidden: map[string]bool{}, failTag: map[string]bool{}, failPush: map[string]bool{}, calls: map[string]int{},
	}
}

func (f *fakeRepo) Exists(_ context.Context, desc ocispec.Descriptor) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.call("Exists")

	_, blob := f.blobs[desc.Digest.String()]
	_, manifest := f.manifests[desc.Digest.String()]

	return blob || manifest, nil
}

func verify(desc ocispec.Descriptor, data []byte) error {
	if int64(len(data)) != desc.Size || digest.FromBytes(data) != desc.Digest.String() {
		return fault.New(fault.Integrity, "data does not match %s", desc.Digest)
	}

	return nil
}

func (f *fakeRepo) PushBlob(_ context.Context, desc ocispec.Descriptor, data []byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.call("PushBlob")

	if err := f.injectedPushFailure(desc); err != nil {
		return false, err
	}

	if err := verify(desc, data); err != nil {
		return false, err
	}

	if _, ok := f.blobs[desc.Digest.String()]; ok {
		return false, nil
	}

	f.blobs[desc.Digest.String()] = slices.Clone(data)
	f.stored = append(f.stored, "blob "+desc.Digest.String())

	return true, nil
}

func (f *fakeRepo) PushManifest(_ context.Context, desc ocispec.Descriptor, data []byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.call("PushManifest")

	if err := f.injectedPushFailure(desc); err != nil {
		return false, err
	}

	if err := verify(desc, data); err != nil {
		return false, err
	}

	if err := f.checkReferences(desc, data); err != nil {
		return false, err
	}

	if _, ok := f.manifests[desc.Digest.String()]; ok {
		return false, nil
	}

	f.manifests[desc.Digest.String()] = slices.Clone(data)
	f.mediaTypes[desc.Digest.String()] = desc.MediaType
	f.stored = append(f.stored, "manifest "+desc.Digest.String())

	return true, nil
}

func (f *fakeRepo) EnsureTag(_ context.Context, desc ocispec.Descriptor, tag string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.call("EnsureTag")

	if current, ok := f.tags[tag]; ok {
		if current == desc.Digest.String() {
			return false, nil
		}

		return false, fault.New(fault.Integrity, "tag %s already points to %s; refusing to move it to %s", tag, current, desc.Digest)
	}

	if err := f.tagLocked(desc, tag); err != nil {
		return false, err
	}

	return true, nil
}

func (f *fakeRepo) Tag(_ context.Context, desc ocispec.Descriptor, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.call("Tag")

	return f.tagLocked(desc, tag)
}

func (f *fakeRepo) Resolve(_ context.Context, tag string) (ocispec.Descriptor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.call("Resolve")

	dgst, ok := f.tags[tag]
	if !ok {
		return ocispec.Descriptor{}, fmt.Errorf("resolve %s: %w", tag, errdef.ErrNotFound)
	}

	return f.descriptor(dgst), nil
}

func (f *fakeRepo) Tags(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.call("Tags")

	tags := []string{}

	for tag := range f.tags {
		if !f.hidden[tag] {
			tags = append(tags, tag)
		}
	}

	slices.Sort(tags)

	return tags, nil
}

func (f *fakeRepo) FetchManifestByDigest(_ context.Context, dgst, mediaType string, limit int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.call("FetchManifestByDigest")

	data, ok := f.manifests[dgst]
	if !ok {
		return nil, fault.Wrap(fault.Registry, fmt.Errorf("manifest %s: %w", dgst, errdef.ErrNotFound), "fetch")
	}

	if f.mediaTypes[dgst] != mediaType {
		return nil, fault.New(fault.Integrity, "manifest %s has media type %q, want %q", dgst, f.mediaTypes[dgst], mediaType)
	}

	if int64(len(data)) > limit {
		return nil, fault.New(fault.Integrity, "manifest too large")
	}

	return slices.Clone(data), nil
}

func (f *fakeRepo) FetchTo(_ context.Context, desc ocispec.Descriptor, w io.Writer) error {
	f.mu.Lock()
	data, ok := f.blobs[desc.Digest.String()]
	if !ok {
		data, ok = f.manifests[desc.Digest.String()]
	}
	f.call("FetchTo")
	f.mu.Unlock()

	if !ok {
		return fault.Wrap(fault.Registry, fmt.Errorf("blob %s: %w", desc.Digest, errdef.ErrNotFound), "fetch")
	}

	if err := verify(desc, data); err != nil {
		return err
	}

	_, err := w.Write(data)

	return err
}

// checkReferences refuses a manifest whose blobs, or an index whose
// manifests, are not present, as registries do.
func (f *fakeRepo) checkReferences(desc ocispec.Descriptor, data []byte) error {
	if desc.MediaType == artifact.IndexMediaType {
		var ix ocispec.Index
		if err := json.Unmarshal(data, &ix); err != nil {
			return fault.Wrap(fault.Registry, err, "index invalid")
		}

		for _, d := range ix.Manifests {
			if _, ok := f.manifests[d.Digest.String()]; !ok {
				return fault.New(fault.Registry, "index manifest unknown: %s", d.Digest)
			}
		}

		return nil
	}

	var m ocispec.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return fault.Wrap(fault.Registry, err, "manifest invalid")
	}

	for _, d := range append([]ocispec.Descriptor{m.Config}, m.Layers...) {
		if _, ok := f.blobs[d.Digest.String()]; !ok {
			return fault.New(fault.Registry, "manifest blob unknown: %s", d.Digest)
		}
	}

	return nil
}

func (f *fakeRepo) call(name string) {
	f.calls[name]++
}

func (f *fakeRepo) count(names ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0
	for _, name := range names {
		n += f.calls[name]
	}

	return n
}

func (f *fakeRepo) writes() int {
	return f.count("PushBlob", "PushManifest", "EnsureTag", "Tag")
}

func (f *fakeRepo) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = map[string]int{}
	f.stored = nil
}

func (f *fakeRepo) injectedPushFailure(desc ocispec.Descriptor) error {
	if !f.failPush[desc.Digest.String()] {
		return nil
	}

	delete(f.failPush, desc.Digest.String())

	return fault.Wrap(fault.Registry, errInjected, "push %s", desc.Digest)
}

func (f *fakeRepo) tagLocked(desc ocispec.Descriptor, tag string) error {
	if f.failTag[tag] {
		delete(f.failTag, tag)

		return fault.Wrap(fault.Registry, errInjected, "tag %s", tag)
	}

	if _, ok := f.manifests[desc.Digest.String()]; !ok {
		return fault.New(fault.Registry, "manifest unknown: %s", desc.Digest)
	}

	f.tags[tag] = desc.Digest.String()

	return nil
}

func (f *fakeRepo) descriptor(dgst string) ocispec.Descriptor {
	desc, err := descriptorOf(f.mediaTypes[dgst], f.manifests[dgst])
	if err != nil {
		panic(err)
	}

	return desc
}

func descriptorOf(mediaType string, data []byte) (ocispec.Descriptor, error) {
	if data == nil {
		return ocispec.Descriptor{}, errors.New("no data")
	}

	var desc ocispec.Descriptor

	desc.MediaType = mediaType
	desc.Digest = digestOf(data)
	desc.Size = int64(len(data))

	return desc, nil
}
