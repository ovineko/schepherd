package wrappers

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// member is a file of a package: its SHA-256 and whether it is executable.
type member struct {
	sum  [sha256.Size]byte
	exec bool
}

// checkPackage reads an npm tarball, wheel or gem and requires exactly the
// files of want, with their bytes and execute bits, plus the files a
// builder generates itself (generated), which may have any content.
func checkPackage(file string, want map[string]member, generated []string) error {
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		return fmt.Errorf("read package: %w", err)
	}

	var got map[string]member

	switch {
	case strings.HasSuffix(file, ".tgz"):
		got, err = readTar(bytes.NewReader(data), true, "package/")
	case strings.HasSuffix(file, ".whl"):
		got, err = readZip(data)
	case strings.HasSuffix(file, ".gem"):
		got, err = readGem(data)
	default:
		err = errors.New("unknown package format")
	}

	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(file), err)
	}

	var problems []string

	for name, m := range got {
		w, ok := want[name]

		switch {
		case !ok && !slices.Contains(generated, name):
			problems = append(problems, name+" is not allowed")
		case ok && w.sum != m.sum:
			problems = append(problems, name+" differs from its source")
		case ok && w.exec != m.exec:
			problems = append(problems, fmt.Sprintf("%s is executable: %t, want %t", name, m.exec, w.exec))
		}
	}

	for _, name := range slices.Concat(slices.Collect(maps.Keys(want)), generated) {
		if _, ok := got[name]; !ok {
			problems = append(problems, name+" is missing")
		}
	}

	if len(problems) > 0 {
		slices.Sort(problems)

		return fmt.Errorf("%s: %s", filepath.Base(file), strings.Join(problems, "; "))
	}

	return nil
}

// maxMember bounds a single file of a package.
const maxMember = 256 << 20

func add(files map[string]member, name string, mode int64, r io.Reader) error {
	if !filepath.IsLocal(filepath.FromSlash(name)) || path.Clean(name) != name {
		return fmt.Errorf("unexpected entry %q", name)
	}

	if _, dup := files[name]; dup {
		return fmt.Errorf("duplicate entry %q", name)
	}

	h := sha256.New()
	if n, err := io.Copy(h, io.LimitReader(r, maxMember+1)); err != nil || n > maxMember {
		return fmt.Errorf("read %s: %w (%d bytes)", name, err, n)
	}

	m := member{exec: mode&0o100 != 0}
	h.Sum(m.sum[:0])
	files[name] = m

	return nil
}

// readTar lists the regular files of a tar archive below prefix; directory
// entries are skipped, anything else is refused. A gzip stream is read to
// its end, where its checksum is verified.
func readTar(r io.Reader, gzipped bool, prefix string) (map[string]member, error) {
	var gz *gzip.Reader

	if gzipped {
		var err error
		if gz, err = gzip.NewReader(r); err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}

		r = gz
	}

	files := map[string]member{}
	tr := tar.NewReader(r)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) && gz != nil {
			if _, err := io.Copy(io.Discard, io.LimitReader(gz, maxMember)); err != nil {
				return nil, fmt.Errorf("gzip: %w", err)
			}
		}

		if errors.Is(err, io.EOF) {
			return files, nil
		}

		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}

		if hdr.Typeflag == tar.TypeDir {
			continue
		}

		name, ok := strings.CutPrefix(hdr.Name, prefix)
		if hdr.Typeflag != tar.TypeReg || !ok {
			return nil, fmt.Errorf("unexpected entry %q", hdr.Name)
		}

		if err := add(files, name, hdr.Mode, tr); err != nil {
			return nil, err
		}
	}
}

func readZip(data []byte) (map[string]member, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}

	files := map[string]member{}

	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("zip: %w", err)
		}

		err = add(files, f.Name, int64(f.Mode().Perm()), rc)
		if closeErr := rc.Close(); err == nil {
			err = closeErr
		}

		if err != nil {
			return nil, err
		}
	}

	return files, nil
}

// readGem lists the files of data.tar.gz, the payload of a gem; the outer
// tar also holds the specification and checksums RubyGems writes itself.
func readGem(data []byte) (map[string]member, error) {
	tr := tar.NewReader(bytes.NewReader(data))

	for {
		hdr, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("no data.tar.gz: %w", err)
		}

		if hdr.Name == "data.tar.gz" {
			return readTar(tr, true, "")
		}
	}
}
