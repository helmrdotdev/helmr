package workspace

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const (
	TreeDigestDomain         = "helmr.workspace-tree.v0\x00"
	CanonicalEmptyTreeDigest = "sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96"
)

const (
	treeEntryDirectory byte = 1
	treeEntryFile      byte = 2
	treeEntrySymlink   byte = 3
)

type TreeIdentity struct {
	Digest     string
	SizeBytes  int64
	EntryCount int
}

func InspectTree(root string) (TreeIdentity, error) {
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return TreeIdentity{}, fmt.Errorf("inspect workspace root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return TreeIdentity{}, errors.New("workspace root must be a directory")
	}

	paths := make([]string, 0)
	err = filepath.WalkDir(root, func(pathname string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if pathname == root {
			return nil
		}
		rel, err := filepath.Rel(root, pathname)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == "" || strings.HasPrefix(rel, "../") || strings.IndexByte(rel, 0) >= 0 {
			return fmt.Errorf("unsafe workspace tree path %q", rel)
		}
		paths = append(paths, rel)
		if len(paths) > MaxArtifactEntries {
			return errors.New("workspace tree contains too many entries")
		}
		return nil
	})
	if err != nil {
		return TreeIdentity{}, err
	}
	sort.Strings(paths)

	digest := sha256.New()
	_, _ = io.WriteString(digest, TreeDigestDomain)
	identity := TreeIdentity{EntryCount: len(paths)}
	for _, rel := range paths {
		pathname := filepath.Join(root, filepath.FromSlash(rel))
		entry, err := inspectTreeEntry(root, pathname, rel)
		if err != nil {
			return TreeIdentity{}, err
		}
		if err := writeTreeRecord(digest, entry, &identity); err != nil {
			return TreeIdentity{}, err
		}
	}
	identity.Digest = sha256sum.DigestHash(digest)
	return identity, nil
}

type treeEntry struct {
	kind   byte
	path   string
	mode   uint32
	size   int64
	target string
	file   string
}

func inspectTreeEntry(root, pathname, rel string) (treeEntry, error) {
	info, err := os.Lstat(pathname)
	if err != nil {
		return treeEntry{}, fmt.Errorf("inspect workspace tree entry %q: %w", rel, err)
	}
	entry := treeEntry{path: rel, mode: uint32(info.Mode().Perm())}
	switch {
	case info.Mode().IsDir():
		entry.kind = treeEntryDirectory
	case info.Mode().IsRegular():
		entry.kind = treeEntryFile
		entry.size = info.Size()
		entry.file = pathname
	case info.Mode()&os.ModeSymlink != 0:
		entry.kind = treeEntrySymlink
		entry.mode = 0o777
		entry.target, err = os.Readlink(pathname)
		if err != nil {
			return treeEntry{}, fmt.Errorf("read workspace symlink %q: %w", rel, err)
		}
		if err := validateTreeSymlink(root, rel, entry.target); err != nil {
			return treeEntry{}, err
		}
	default:
		return treeEntry{}, fmt.Errorf("unsupported workspace tree entry %q", rel)
	}
	return entry, nil
}

func validateTreeSymlink(root, rel, target string) error {
	if target == "" || strings.IndexByte(target, 0) >= 0 || filepath.IsAbs(target) {
		return fmt.Errorf("unsafe workspace symlink %q target %q", rel, target)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(rel)), target))
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return fmt.Errorf("workspace symlink %q escapes root", rel)
	}
	root = filepath.Clean(root)
	if candidate := filepath.Join(root, resolved); candidate != root && !strings.HasPrefix(candidate, root+string(filepath.Separator)) {
		return fmt.Errorf("workspace symlink %q escapes root", rel)
	}
	return nil
}

func writeTreeRecord(writer io.Writer, entry treeEntry, identity *TreeIdentity) error {
	if uint64(len(entry.path)) > uint64(^uint32(0)) {
		return errors.New("workspace tree path is too long")
	}
	payloadLength := uint64(0)
	switch entry.kind {
	case treeEntryFile:
		if entry.size < 0 {
			return fmt.Errorf("workspace file %q has invalid size", entry.path)
		}
		if entry.size > MaxArtifactExtractedBytes || identity.SizeBytes > MaxArtifactExtractedBytes-entry.size {
			return errors.New("workspace tree exceeds byte limit")
		}
		payloadLength = uint64(entry.size)
		identity.SizeBytes += entry.size
	case treeEntrySymlink:
		payloadLength = uint64(len(entry.target))
	case treeEntryDirectory:
	default:
		return fmt.Errorf("unsupported workspace tree entry %q", entry.path)
	}

	if _, err := writer.Write([]byte{entry.kind}); err != nil {
		return err
	}
	if err := writeTreeUint32(writer, uint32(len(entry.path))); err != nil {
		return err
	}
	if _, err := io.WriteString(writer, entry.path); err != nil {
		return err
	}
	if err := writeTreeUint32(writer, entry.mode); err != nil {
		return err
	}
	if err := writeTreeUint64(writer, payloadLength); err != nil {
		return err
	}

	switch entry.kind {
	case treeEntryFile:
		return writeTreeFile(writer, entry)
	case treeEntrySymlink:
		_, err := io.WriteString(writer, entry.target)
		return err
	default:
		return nil
	}
}

func writeTreeFile(writer io.Writer, entry treeEntry) error {
	file, err := os.Open(entry.file)
	if err != nil {
		return fmt.Errorf("open workspace file %q: %w", entry.path, err)
	}
	defer file.Close()
	if _, err := io.CopyN(writer, file, entry.size); err != nil {
		return fmt.Errorf("read workspace file %q: %w", entry.path, err)
	}
	var extra [1]byte
	if count, err := file.Read(extra[:]); err != io.EOF || count != 0 {
		if err == nil {
			return fmt.Errorf("workspace file %q changed while inspecting", entry.path)
		}
		return fmt.Errorf("finish workspace file %q: %w", entry.path, err)
	}
	return nil
}

func writeTreeUint32(writer io.Writer, value uint32) error {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	_, err := writer.Write(encoded[:])
	return err
}

func writeTreeUint64(writer io.Writer, value uint64) error {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, err := writer.Write(encoded[:])
	return err
}
