package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/fault"
)

func exactArgs(n int, usage string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return fault.New(fault.Usage, "expected %s, got %d argument(s)", usage, len(args))
		}

		return nil
	}
}

// withSchema prepares one schema, a local file or a materialized catalog
// artifact, and hands it to fn.
func (a *app) withSchema(ctx context.Context, id string, fn func(preparedSchema) error) error {
	ctx, cancel, err := a.withTimeout(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	src, err := a.openSources(ctx)
	if err != nil {
		return err
	}
	defer src.close()

	sch, err := src.prepare(ctx, id)
	if err != nil {
		return err
	}

	return fn(sch)
}

func (a *app) newPathCmd() *cobra.Command {
	var nul bool

	cmd := &cobra.Command{
		Use:   "path <id>",
		Short: "Print the absolute path of the verified schema file (materialized, or local in place)",
		Args:  exactArgs(1, "one schema id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withSchema(cmd.Context(), args[0], func(sch preparedSchema) error {
				terminator := "\n"
				if nul {
					terminator = "\x00"
				}

				return a.write(sch.info.Path + terminator)
			})
		},
	}
	cmd.Flags().BoolVar(&nul, "null", false, "terminate the path with NUL instead of a newline")

	return cmd
}

func (a *app) newCatCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cat <id>",
		Short: "Write the schema JSON bytes to standard output",
		Args:  exactArgs(1, "one schema id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withSchema(cmd.Context(), args[0], func(sch preparedSchema) error {
				data, err := sch.content()
				if err != nil {
					return err
				}

				return a.writeBytes(data)
			})
		},
	}
}

// noticeSuffix names the file next to an exported schema that holds the
// notice published with it.
const noticeSuffix = ".NOTICE"

// exportResult is the --json output of export.
type exportResult struct {
	ID     string `json:"id"`
	Origin string `json:"origin"`
	Path   string `json:"path"`
	Notice string `json:"notice,omitempty"`
}

func (a *app) newExportCmd() *cobra.Command {
	var force, asJSON bool

	cmd := &cobra.Command{
		Use:   "export <id> <destination>",
		Short: "Write an independent copy of the verified schema, and its notice, to files",
		Args:  exactArgs(2, "a schema id and a destination path"),
		RunE: func(cmd *cobra.Command, args []string) error {
			dest, err := filepath.Abs(args[1])
			if err != nil {
				return fault.Wrap(fault.Usage, err, "destination %q", args[1])
			}

			if !force {
				for _, p := range []string{dest, dest + noticeSuffix} {
					if _, err := os.Lstat(p); err == nil {
						return fault.New(fault.Usage, "%s already exists; pass --force to replace it", p)
					}
				}
			}

			return a.withSchema(cmd.Context(), args[0], func(sch preparedSchema) error {
				data, err := sch.content()
				if err != nil {
					return err
				}

				notice, err := sch.notice()
				if err != nil {
					return err
				}

				if err := exportSchema(dest, data, notice, force); err != nil {
					return err
				}

				if !asJSON {
					return nil
				}

				res := exportResult{ID: sch.info.ID, Origin: sch.info.Origin, Path: dest}
				if notice != nil {
					res.Notice = dest + noticeSuffix
				}

				return a.writeJSON(res)
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing destination file and its notice file (a symlink is replaced, not followed)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the written files as JSON")

	return cmd
}

// exportSchema writes the schema to dest and its notice to dest.NOTICE.
// Both destinations are checked before either file changes. The notice is
// installed first, so a complete schema file never lacks the attribution
// that belongs to it; when the schema then cannot be put in place, the
// notice installed here is removed again (only if it is still this run's
// file) and, with force, the previous notice is restored. With force, a
// stale notice of an earlier export is removed after a schema without a
// notice is in place, so it cannot attribute the wrong content.
func exportSchema(dest string, data, notice []byte, force bool) error {
	noticeDest := dest + noticeSuffix

	for _, p := range []string{dest, noticeDest} {
		if err := checkExportDest(p, force); err != nil {
			return err
		}
	}

	if notice == nil {
		if err := writeExport(dest, data, force); err != nil {
			return err
		}

		if force {
			return removeStaleNotice(noticeDest)
		}

		return nil
	}

	previous := ""

	if force {
		var err error
		if previous, err = setAside(noticeDest); err != nil {
			return err
		}
	}

	installed, err := installExport(noticeDest, notice, force)
	if err != nil {
		restoreNotice(noticeDest, previous)

		return err
	}

	if err := writeExport(dest, data, force); err != nil {
		if current, statErr := os.Lstat(noticeDest); statErr == nil && os.SameFile(current, installed) {
			_ = os.Remove(noticeDest)
		}

		restoreNotice(noticeDest, previous)

		return err
	}

	if previous != "" {
		_ = os.Remove(previous)
	}

	return nil
}

// checkExportDest refuses a destination that is a directory, or that exists
// when force is not set.
func checkExportDest(dest string, force bool) error {
	info, err := os.Lstat(dest)

	switch {
	case err == nil && info.IsDir():
		return fault.New(fault.Usage, "%s is a directory", dest)
	case err == nil && !force:
		return fault.New(fault.Usage, "%s already exists; pass --force to replace it", dest)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fault.Wrap(fault.Usage, err, "destination %s", dest)
	}

	return nil
}

// setAside moves an existing notice file or symlink (never its target) to a
// temporary name next to it and returns that name, or "" when there is none.
func setAside(path string) (string, error) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".schepherd-export-previous-*")
	if err != nil {
		return "", fault.Wrap(fault.Usage, err, "create a temporary file next to %s", path)
	}

	name := tmp.Name()
	_ = tmp.Close()

	if err := os.Rename(path, name); err != nil {
		_ = os.Remove(name)

		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}

		return "", fault.Wrap(fault.Usage, err, "move the previous notice %s aside", path)
	}

	return name, nil
}

// removeStaleNotice removes a file or symlink (never its target) at path;
// checkExportDest has already refused a directory there.
func removeStaleNotice(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fault.Wrap(fault.Usage, err, "remove the stale notice %s", path)
	}

	return nil
}

// restoreNotice puts a notice that setAside moved away back in place.
func restoreNotice(path, previous string) {
	if previous != "" {
		_ = os.Rename(previous, path)
	}
}

// linkFile is replaced by tests to simulate filesystems without hard links.
var linkFile = os.Link

// writeExport writes data next to dest and moves it into place atomically.
// Without force it links instead of renaming, which fails if dest appeared
// in the meantime, so an existing file is never overwritten; where the
// filesystem cannot link, it creates dest exclusively instead.
func writeExport(dest string, data []byte, force bool) error {
	_, err := installExport(dest, data, force)

	return err
}

// installExport is writeExport that also returns the identity of the file it
// put in place, so a later rollback can tell it from a file another process
// installed at the same path.
func installExport(dest string, data []byte, force bool) (fs.FileInfo, error) {
	if err := checkExportDest(dest, force); err != nil {
		return nil, err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".schepherd-export-*")
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "create a temporary file next to %s", dest)
	}

	tmpName := tmp.Name()

	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return nil, fault.Wrap(fault.Internal, err, "write %s", tmpName)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()

		return nil, fault.Wrap(fault.Internal, err, "sync %s", tmpName)
	}

	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()

		return nil, fault.Wrap(fault.Internal, err, "chmod %s", tmpName)
	}

	if err := tmp.Close(); err != nil {
		return nil, fault.Wrap(fault.Internal, err, "close %s", tmpName)
	}

	// The temporary file and the installed one are the same file after a
	// rename or a link.
	info, err := os.Lstat(tmpName)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "stat %s", tmpName)
	}

	if force {
		if err := os.Rename(tmpName, dest); err != nil {
			return nil, fault.Wrap(fault.Usage, err, "replace %s", dest)
		}

		return info, nil
	}

	linkErr := linkFile(tmpName, dest)
	if linkErr == nil {
		return info, nil
	}

	if errors.Is(linkErr, fs.ErrExist) {
		return nil, fault.New(fault.Usage, "%s already exists; pass --force to replace it", dest)
	}

	// FAT, exFAT and many network or FUSE mounts cannot link, and they report
	// it with different errors (EPERM, EOPNOTSUPP, ERROR_INVALID_FUNCTION).
	// An exclusive create keeps the no-clobber guarantee on any filesystem;
	// only the atomic appearance of the complete file is lost.
	info, err = createExclusive(dest, data)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fault.New(fault.Usage, "%s already exists; pass --force to replace it", dest)
		}

		return nil, fault.Wrap(fault.Usage, fmt.Errorf("%w; %w", linkErr, err), "create %s", dest)
	}

	return info, nil
}

// createExclusive creates dest, failing if anything exists there, and
// removes it again when the content cannot be written completely. It returns
// the identity of the created file.
func createExclusive(dest string, data []byte) (fs.FileInfo, error) {
	// bearer:disable go_gosec_file_permissions_file_perm,go_gosec_filesystem_filereadtaint
	// dest is the export destination the user named, and docs/cli.md documents
	// mode 0644 for the exported schema, a public document.
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // dest is the export destination the user named
	if err != nil {
		return nil, fmt.Errorf("exclusive create: %w", err)
	}

	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}

	if err == nil {
		err = f.Chmod(0o644)
	}

	var info fs.FileInfo
	if err == nil {
		info, err = f.Stat()
	}

	if closeErr := f.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		_ = os.Remove(dest)

		return nil, fmt.Errorf("write %s: %w", dest, err)
	}

	return info, nil
}

func (a *app) write(s string) error {
	return a.writeBytes([]byte(s))
}

func (a *app) writeBytes(b []byte) error {
	if _, err := a.stdout.Write(b); err != nil {
		return fault.Wrap(fault.Internal, err, "write to standard output")
	}

	return nil
}

func (a *app) writeLine(format string, args ...any) error {
	return a.write(fmt.Sprintf(format, args...) + "\n")
}
