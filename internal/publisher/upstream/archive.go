package upstream

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/httpfetch"
)

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// archiveLimits bound what an upstream tarball may make the publisher read and
// write. The pinned SchemaStore commit has about 5,400 entries, 90 MB of tar
// data, 84 MB of selected files and no file above 8 MB.
type archiveLimits struct {
	maxEntries        int
	maxTotalBytes     int64
	maxExtractedBytes int64
	maxFileBytes      int64
}

var defaultArchiveLimits = archiveLimits{
	maxEntries:        50000,
	maxTotalBytes:     1 << 30,
	maxExtractedBytes: 512 << 20,
	maxFileBytes:      64 << 20,
}

// maxTarPadding bounds the zero bytes allowed after the tar end-of-archive
// marker. Tar writers pad to their record size (git archive: 10 KiB); 1 MiB
// covers blocking factors up to 2048.
const maxTarPadding = 1 << 20

// FetchSchemaStore downloads the SchemaStore tarball of commit (a full,
// lowercase 40-hex SHA) from baseURL/<commit> (tarball_base_url of the
// upstream description) with f, whose MaxBytes must admit the compressed
// tarball, and extracts the snapshot into dest, which must not exist yet.
//
// Only src/api/json/catalog.json, src/schemas/json/*.json, src/test/**,
// src/negative_test/**, LICENSE and NOTICE are extracted. Every entry must
// live under the "schemastore-<commit>/" top directory; absolute paths, ".."
// segments, links, devices and FIFOs are refused, and entry counts and sizes
// are bounded. A pax global header naming another commit is refused, and so
// are a gzip trailer that does not match the data, bytes after the gzip
// member and anything but zero padding after the end of the tar archive.
// Extraction happens in a sibling temporary directory that is renamed to
// dest only after it succeeded, so dest never holds a partial snapshot.
func FetchSchemaStore(ctx context.Context, f *httpfetch.Fetcher, baseURL, commit, dest string) (*Snapshot, error) {
	return fetchSchemaStore(ctx, f, baseURL, commit, dest, defaultArchiveLimits)
}

func fetchSchemaStore(
	ctx context.Context, f *httpfetch.Fetcher, baseURL, commit, dest string, limits archiveLimits,
) (*Snapshot, error) {
	if !commitPattern.MatchString(commit) {
		return nil, fault.New(fault.Usage, "commit %q must be a full lowercase 40-hex SHA", commit)
	}

	dest, err := filepath.Abs(dest)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "snapshot directory")
	}

	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		return nil, fault.New(fault.Usage, "snapshot directory %s already exists", dest)
	}

	res, err := f.Get(ctx, strings.TrimRight(baseURL, "/")+"/"+commit)
	if err != nil {
		return nil, fault.Wrap(fault.Registry, err, "download SchemaStore %s", commit)
	}

	tmp, err := os.MkdirTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".tmp-")
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "create temporary directory next to %s", dest)
	}

	snap, err := populate(ctx, tmp, res, commit, limits)
	if err != nil {
		_ = os.RemoveAll(tmp)

		return nil, err
	}

	if err := os.Rename(tmp, dest); err != nil {
		_ = os.RemoveAll(tmp)

		return nil, fault.Wrap(fault.Internal, err, "move snapshot into place")
	}

	snap.Dir = dest

	return snap, nil
}

// populate extracts into dir and opens the result, so an archive that lacks
// required files is rejected before it is moved into place.
func populate(ctx context.Context, dir string, res httpfetch.Result, commit string, limits archiveLimits) (*Snapshot, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "open temporary snapshot directory")
	}

	defer func() { _ = root.Close() }()

	if err := extract(ctx, res.Body, commit, root, limits); err != nil {
		return nil, err
	}

	meta, err := json.Marshal(snapshotMeta{FormatVersion: snapshotFormatVersion, Commit: commit, TarballDigest: res.Digest})
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "encode snapshot metadata")
	}

	if err := root.WriteFile(metaFile, append(meta, '\n'), 0o644); err != nil {
		return nil, fault.Wrap(fault.Internal, err, "write snapshot metadata")
	}

	snap, err := OpenSnapshot(dir)
	if err != nil {
		return nil, fault.Reclassify(fault.Integrity, err, "SchemaStore tarball is incomplete")
	}

	return snap, nil
}

type extractor struct {
	root      *os.Root
	tr        *tar.Reader
	top       string
	commit    string
	limits    archiveLimits
	entries   int
	total     int64
	extracted int64
}

func extract(ctx context.Context, tarball []byte, commit string, root *os.Root, limits archiveLimits) error {
	r := bytes.NewReader(tarball)

	gz, err := gzip.NewReader(r)
	if err != nil {
		return fault.Wrap(fault.Integrity, err, "SchemaStore tarball is not gzip")
	}

	defer func() { _ = gz.Close() }()

	gz.Multistream(false)

	x := &extractor{root: root, tr: tar.NewReader(gz), top: "schemastore-" + commit, commit: commit, limits: limits}

	for {
		if err := ctx.Err(); err != nil {
			return fault.Wrap(fault.Canceled, err, "extract SchemaStore tarball")
		}

		h, err := x.tr.Next()
		if errors.Is(err, io.EOF) {
			return finish(gz, r)
		}

		if err != nil {
			return fault.Wrap(fault.Integrity, err, "read SchemaStore tarball")
		}

		if err := x.entry(h); err != nil {
			return err
		}
	}
}

// finish reads the rest of the gzip member. archive/tar stops at the
// end-of-archive marker, so without this the gzip CRC-32 and length trailer
// would never be checked. gzip reads r byte by byte (bytes.Reader is an
// io.ByteReader), so r is positioned right after the member.
func finish(gz *gzip.Reader, r *bytes.Reader) error {
	n, err := io.Copy(zeroPadding{}, io.LimitReader(gz, maxTarPadding+1))

	switch {
	case errors.Is(err, errNotPadding):
		return fault.New(fault.Integrity, "SchemaStore tarball has data after the end of the tar archive")
	case err != nil:
		return fault.Wrap(fault.Integrity, err, "read SchemaStore tarball")
	case n > maxTarPadding:
		return fault.New(fault.Integrity, "SchemaStore tarball has more than %d bytes of padding", maxTarPadding)
	case r.Len() != 0:
		return fault.New(fault.Integrity, "SchemaStore tarball has %d bytes after the gzip stream", r.Len())
	}

	return nil
}

var errNotPadding = errors.New("non-zero byte after the end of the archive")

type zeroPadding struct{}

func (zeroPadding) Write(p []byte) (int, error) {
	if len(bytes.Trim(p, "\x00")) != 0 {
		return 0, errNotPadding
	}

	return len(p), nil
}

func (x *extractor) entry(h *tar.Header) error {
	x.entries++
	if x.entries > x.limits.maxEntries {
		return fault.New(fault.Integrity, "SchemaStore tarball has more than %d entries", x.limits.maxEntries)
	}

	switch h.Typeflag {
	case tar.TypeXGlobalHeader:
		if comment, ok := h.PAXRecords["comment"]; ok && comment != x.commit {
			return fault.New(fault.Integrity, "SchemaStore tarball is for commit %q, expected %s", comment, x.commit)
		}

		return nil
	case tar.TypeReg, tar.TypeDir:
	default:
		return fault.New(fault.Integrity, "SchemaStore tarball entry %q has type %q; only files and directories are accepted",
			h.Name, h.Typeflag)
	}

	rel, err := x.relative(h.Name, h.Typeflag == tar.TypeDir)
	if err != nil {
		return err
	}

	if h.Size < 0 || h.Size > x.limits.maxTotalBytes-x.total {
		return fault.New(fault.Integrity, "SchemaStore tarball holds more than %d bytes", x.limits.maxTotalBytes)
	}

	x.total += h.Size

	if h.Typeflag == tar.TypeDir || !selected(rel) {
		return nil
	}

	if h.Size > x.limits.maxFileBytes {
		return fault.New(fault.Integrity, "SchemaStore file %s is larger than %d bytes", rel, x.limits.maxFileBytes)
	}

	if h.Size > x.limits.maxExtractedBytes-x.extracted {
		return fault.New(fault.Integrity, "SchemaStore snapshot would exceed %d bytes", x.limits.maxExtractedBytes)
	}

	x.extracted += h.Size

	return x.write(rel, h.Size)
}

func (x *extractor) relative(name string, isDir bool) (string, error) {
	clean := name
	if isDir {
		clean = strings.TrimSuffix(name, "/")
	}

	if err := checkEntryName(clean); err != nil {
		return "", fault.Wrap(fault.Integrity, err, "SchemaStore tarball entry %q", name)
	}

	if clean == x.top {
		if !isDir {
			return "", fault.New(fault.Integrity, "SchemaStore tarball entry %q must be a directory", name)
		}

		return "", nil
	}

	rel, ok := strings.CutPrefix(clean, x.top+"/")
	if !ok {
		return "", fault.New(fault.Integrity, "SchemaStore tarball entry %q is outside %s/", name, x.top)
	}

	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", fault.New(fault.Integrity, "SchemaStore tarball entry %q is not a local path on this system", name)
	}

	return rel, nil
}

// checkEntryName accepts clean, relative, slash-separated names whose
// characters are valid in file names on every supported platform.
func checkEntryName(name string) error {
	switch {
	case name == "":
		return errors.New("empty name")
	case !utf8.ValidString(name):
		return errors.New("name is not valid UTF-8")
	case strings.HasPrefix(name, "/"):
		return errors.New("absolute path")
	case path.Clean(name) != name:
		return errors.New("path is not clean")
	case strings.ContainsAny(name, `\:*?"<>|`):
		return errors.New("name contains characters that are not portable")
	}

	for segment := range strings.SplitSeq(name, "/") {
		if segment == "." || segment == ".." {
			return errors.New("path contains dot segments")
		}
	}

	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("name contains control characters")
		}
	}

	return nil
}

func selected(rel string) bool {
	switch {
	case rel == catalogFile, rel == licenseFile, rel == noticeFile:
		return true
	case path.Dir(rel) == schemasDir:
		return strings.HasSuffix(rel, ".json")
	default:
		return strings.HasPrefix(rel, positiveDir+"/") || strings.HasPrefix(rel, negativeDir+"/")
	}
}

func (x *extractor) write(rel string, size int64) error {
	name := filepath.FromSlash(rel)

	if dir := filepath.Dir(name); dir != "." {
		if err := x.root.MkdirAll(dir, 0o755); err != nil {
			return fault.Wrap(fault.Integrity, err, "create directory for %s", rel)
		}
	}

	file, err := x.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fault.New(fault.Integrity, "SchemaStore tarball contains %s twice", rel)
		}

		return fault.Wrap(fault.Internal, err, "create %s", rel)
	}

	n, copyErr := io.Copy(file, x.tr)
	closeErr := file.Close()

	switch {
	case copyErr != nil:
		return fault.Wrap(fault.Integrity, copyErr, "extract %s", rel)
	case n != size:
		return fault.New(fault.Integrity, "extract %s: got %d of %d bytes", rel, n, size)
	case closeErr != nil:
		return fault.Wrap(fault.Internal, closeErr, "write %s", rel)
	}

	return nil
}
