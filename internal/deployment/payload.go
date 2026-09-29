package deployment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/safepath"
)

// ProgramPayloadDigest freezes the identity of every payload file before
// any customer code executes. Its inventory uses the canonical archive modes;
// admission uses the same hash function over the inspected archive inventory.
func ProgramPayloadDigest(ctx context.Context, root string) (string, error) {
	if ctx == nil {
		return "", errors.New("program input digest context is nil")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	confined, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer confined.Close()
	entries := make([]artifact.Entry, 0)
	var total, nameBytes int64
	var collect func(string) error
	collect = func(relative string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(entries) >= artifact.MaxProgramTreeEntries {
			return errors.New("installed input exceeds Program entry bounds")
		}
		if relative != "." {
			if err := safepath.ValidateTreePath(relative, artifact.ProgramMountPath, "/workspace/project", "/workspace/program"); err != nil {
				return err
			}
		}
		if err := safepath.ValidateHostTreePath(root, relative); err != nil {
			return err
		}
		info, err := confined.Lstat(relative)
		if err != nil {
			return err
		}
		entry := artifact.Entry{Path: relative}
		switch {
		case info.Mode().IsRegular():
			entry.Kind = artifact.EntryRegular
			entry.Mode = 0644
			if info.Mode().Perm()&0111 != 0 {
				entry.Mode = 0755
			}
			entry.SizeBytes = info.Size()
			total += info.Size()
			if info.Size() > artifact.MaxFileSize || total > artifact.MaxProgramLogicalBytes {
				return fmt.Errorf("installed input %q exceeds Program file or tree size bounds", relative)
			}
		case info.IsDir():
			entry.Kind = artifact.EntryDirectory
			entry.Mode = 0755
		case info.Mode()&os.ModeSymlink != 0:
			entry.Kind = artifact.EntrySymlink
			entry.Mode = 0777
			entry.LinkTarget, err = confined.Readlink(relative)
			if err != nil {
				return err
			}
			if err := artifact.ValidateLinkTarget(entry.LinkTarget); err != nil {
				return fmt.Errorf("input symlink %q: %w", relative, err)
			}
		default:
			return fmt.Errorf("unsupported installed input %q", relative)
		}
		nameBytes, err = artifact.ChargeNameBytes(nameBytes, entry)
		if err != nil {
			return err
		}
		entries = append(entries, entry)
		if !info.IsDir() {
			return nil
		}
		directory, err := confined.Open(relative)
		if err != nil {
			return err
		}
		defer directory.Close()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			children, readErr := directory.ReadDir(128)
			for _, child := range children {
				childName := child.Name()
				if relative != "." {
					childName = relative + "/" + childName
				}
				if err := collect(childName); err != nil {
					return err
				}
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	if err := collect("."); err != nil {
		return "", err
	}

	byPath := make(map[string]artifact.Entry, len(entries))
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	lookup := func(path string) (artifact.Entry, bool) {
		entry, exists := byPath[path]
		return entry, exists
	}
	if err := validateBuildTreeLinks(entries, lookup); err != nil {
		return "", err
	}
	return artifact.PayloadDigest(ctx, entries, func(ctx context.Context, name string) (io.ReadCloser, error) {
		return confined.Open(name)
	})
}
