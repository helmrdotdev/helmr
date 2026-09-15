package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

// ProgramInputTreeDigest freezes the identity of every installed input before
// any customer code executes. Its inventory uses the canonical archive modes;
// admission uses the same hash function over the inspected archive inventory.
func ProgramInputTreeDigest(ctx context.Context, root string) (string, error) {
	if ctx == nil {
		return "", errors.New("program input digest context is nil")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	entries := make([]artifactEntry, 0)
	var total, nameBytes int64
	err = filepath.WalkDir(root, func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == "helmr" {
			return errors.New("installed input contains reserved root helmr path")
		}
		if relative != "." {
			if err := validateArtifactPath(relative, programArtifact); err != nil {
				return err
			}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := artifactEntry{Path: relative}
		switch {
		case info.Mode().IsRegular():
			entry.Kind = artifactEntryRegular
			entry.Mode = 0644
			if info.Mode().Perm()&0111 != 0 {
				entry.Mode = 0755
			}
			entry.SizeBytes = info.Size()
			total += info.Size()
			if info.Size() > maxArtifactFileSize || total > maxProgramLogicalBytes {
				return fmt.Errorf("installed input %q exceeds Program file or tree size bounds", relative)
			}
		case info.IsDir():
			entry.Kind = artifactEntryDirectory
			entry.Mode = 0755
		case info.Mode()&os.ModeSymlink != 0:
			entry.Kind = artifactEntrySymlink
			entry.Mode = 0777
			entry.LinkTarget, err = os.Readlink(name)
			if err != nil {
				return err
			}
			if err := validateSymlinkTarget(entry.LinkTarget); err != nil {
				return fmt.Errorf("input symlink %q: %w", relative, err)
			}
		default:
			return fmt.Errorf("unsupported installed input %q", relative)
		}
		nameBytes, err = chargeArtifactNameBytes(nameBytes, entry)
		if err != nil {
			return err
		}
		entries = append(entries, entry)
		if len(entries) > MaxProgramTreeEntries {
			return errors.New("installed input exceeds Program entry bounds")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	tree := &inspectedArtifact{ordered: entries, entries: make(map[string]artifactEntry, len(entries))}
	for _, entry := range entries {
		tree.entries[entry.Path] = entry
	}
	if err := validateBuildTreeLinks(tree); err != nil {
		return "", err
	}
	return inputTreeDigest(ctx, entries, func(ctx context.Context, name string) (io.ReadCloser, error) {
		return os.Open(filepath.Join(root, filepath.FromSlash(name)))
	})
}

func artifactInputTreeDigest(ctx context.Context, artifact *inspectedArtifact) (string, error) {
	return inputTreeDigest(ctx, artifact.ordered, artifact.reader.Open)
}

func inputTreeDigest(ctx context.Context, entries []artifactEntry, open func(context.Context, string) (io.ReadCloser, error)) (string, error) {
	ordered := append([]artifactEntry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	hash := sha256.New()
	_, _ = io.WriteString(hash, "helmr.program-input-tree.v0\n")
	for _, entry := range ordered {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if entry.Path == "helmr" || strings.HasPrefix(entry.Path, "helmr/") {
			continue
		}
		record := map[string]any{"path": entry.Path, "kind": entry.Kind, "mode": entry.Mode}
		switch entry.Kind {
		case artifactEntryDirectory:
		case artifactEntrySymlink:
			record["linkTarget"] = entry.LinkTarget
		case artifactEntryRegular:
			reader, err := open(ctx, entry.Path)
			if err != nil {
				return "", err
			}
			content := sha256.New()
			size, readErr := io.Copy(content, io.LimitReader(reader, entry.SizeBytes+1))
			closeErr := reader.Close()
			if err := errors.Join(readErr, closeErr); err != nil {
				return "", err
			}
			if size != entry.SizeBytes {
				return "", fmt.Errorf("input %q changed size during digest", entry.Path)
			}
			record["digest"] = sha256sum.FormatDigest(content.Sum(nil))
			record["sizeBytes"] = size
		default:
			return "", fmt.Errorf("invalid input kind at %q", entry.Path)
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return "", err
		}
		canonical, err := jsoncanon.Transform(raw)
		if err != nil {
			return "", err
		}
		_, _ = hash.Write(canonical)
		_, _ = hash.Write([]byte{'\n'})
	}
	return sha256sum.FormatDigest(hash.Sum(nil)), nil
}
