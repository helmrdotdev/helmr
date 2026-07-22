package workspace

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func VerifyArtifact(body io.Reader, artifact WorkspaceArtifact, reportedTree TreeIdentity) error {
	if artifact.Digest == "" || artifact.MediaType != ArtifactMediaType || artifact.Encoding != ArtifactEncoding ||
		artifact.SizeBytes <= 0 || artifact.SizeBytes > MaxArtifactArchiveBytes ||
		artifact.EntryCount < 0 || artifact.EntryCount > MaxArtifactEntries {
		return errors.New("Workspace Artifact descriptor is invalid")
	}
	verificationRoot, err := os.MkdirTemp("", "helmr-workspace-artifact-*")
	if err != nil {
		return fmt.Errorf("create Workspace Artifact verification root: %w", err)
	}
	defer os.RemoveAll(verificationRoot)
	archiveFile, err := os.CreateTemp(verificationRoot, "artifact-*.tar")
	if err != nil {
		return fmt.Errorf("create Workspace Artifact verification file: %w", err)
	}
	archivePath := archiveFile.Name()
	hash := sha256.New()
	written, copyErr := io.CopyN(io.MultiWriter(archiveFile, hash), body, artifact.SizeBytes)
	var extra [1]byte
	extraCount, extraErr := body.Read(extra[:])
	closeErr := archiveFile.Close()
	if copyErr != nil || written != artifact.SizeBytes {
		return errors.New("Workspace Artifact ended before its declared size")
	}
	if extraErr != io.EOF || extraCount != 0 {
		return errors.New("Workspace Artifact exceeds its declared size")
	}
	if closeErr != nil {
		return fmt.Errorf("close Workspace Artifact verification file: %w", closeErr)
	}
	if sha256sum.DigestHash(hash) != artifact.Digest {
		return errors.New("Workspace Artifact bytes do not match its digest")
	}

	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open Workspace Artifact verification file: %w", err)
	}
	tree, inspectErr := inspectArtifactTree(file, artifact.SizeBytes)
	fileCloseErr := file.Close()
	if inspectErr != nil {
		return inspectErr
	}
	if fileCloseErr != nil {
		return fmt.Errorf("close Workspace Artifact: %w", fileCloseErr)
	}
	if tree.EntryCount != artifact.EntryCount {
		return errors.New("Workspace Artifact entry count does not match its descriptor")
	}
	if tree != reportedTree {
		return errors.New("Workspace Artifact tree does not match its receipt")
	}
	return nil
}

func InspectArtifactTree(path string, sizeBytes int64) (TreeIdentity, error) {
	return InspectArtifactTreeContext(context.Background(), path, sizeBytes)
}

func InspectArtifactTreeContext(ctx context.Context, path string, sizeBytes int64) (TreeIdentity, error) {
	if err := ctx.Err(); err != nil {
		return TreeIdentity{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return TreeIdentity{}, fmt.Errorf("open Workspace Artifact: %w", err)
	}
	defer file.Close()
	return inspectArtifactTreeContext(ctx, file, sizeBytes)
}

func inspectArtifactTree(file *os.File, archiveSize int64) (TreeIdentity, error) {
	return inspectArtifactTreeContext(context.Background(), file, archiveSize)
}

func inspectArtifactTreeContext(ctx context.Context, file *os.File, archiveSize int64) (TreeIdentity, error) {
	reader := tar.NewReader(contextReader{ctx: ctx, reader: file})
	digest := sha256.New()
	_, _ = io.WriteString(digest, TreeDigestDomain)
	identity := TreeIdentity{}
	previousPath := ""
	directories := make(map[string]struct{})
	for {
		if err := ctx.Err(); err != nil {
			return TreeIdentity{}, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if err := ctx.Err(); err != nil {
				return TreeIdentity{}, err
			}
			position, seekErr := file.Seek(0, io.SeekCurrent)
			if seekErr != nil {
				return TreeIdentity{}, fmt.Errorf("inspect Workspace Artifact envelope: %w", seekErr)
			}
			if position != archiveSize {
				return TreeIdentity{}, errors.New("Workspace Artifact contains trailing bytes")
			}
			identity.Digest = sha256sum.DigestHash(digest)
			return identity, nil
		}
		if err != nil {
			return TreeIdentity{}, fmt.Errorf("read Workspace Artifact: %w", err)
		}
		name := header.Name
		if name == "" || strings.IndexByte(name, 0) >= 0 || path.IsAbs(name) || path.Clean(name) != name ||
			name == "." || name == ".." || strings.HasPrefix(name, "../") ||
			(previousPath != "" && name <= previousPath) {
			return TreeIdentity{}, fmt.Errorf("Workspace Artifact path %q is invalid or out of order", name)
		}
		previousPath = name
		parent := path.Dir(name)
		if parent != "." {
			if _, ok := directories[parent]; !ok {
				return TreeIdentity{}, fmt.Errorf("Workspace Artifact entry %q has no directory parent", name)
			}
		}
		identity.EntryCount++
		if identity.EntryCount > MaxArtifactEntries {
			return TreeIdentity{}, errors.New("Workspace Artifact contains too many entries")
		}
		if header.Mode < 0 || header.Mode&^0o777 != 0 {
			return TreeIdentity{}, fmt.Errorf("Workspace Artifact entry %q has unsupported mode", name)
		}
		mode := uint32(header.Mode)
		var kind byte
		var payloadLength uint64
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 || header.Linkname != "" {
				return TreeIdentity{}, fmt.Errorf("Workspace Artifact directory %q is invalid", name)
			}
			kind = treeEntryDirectory
			directories[name] = struct{}{}
		case tar.TypeReg:
			if header.Linkname != "" {
				return TreeIdentity{}, fmt.Errorf("Workspace Artifact file %q is invalid", name)
			}
			if err := archive.ValidateTarRegularFileSize(header, &identity.SizeBytes, MaxArtifactExtractedBytes); err != nil {
				return TreeIdentity{}, fmt.Errorf("Workspace Artifact file %q is invalid: %w", name, err)
			}
			kind = treeEntryFile
			payloadLength = uint64(header.Size)
		case tar.TypeSymlink:
			if header.Size != 0 || header.Linkname == "" || strings.IndexByte(header.Linkname, 0) >= 0 || path.IsAbs(header.Linkname) {
				return TreeIdentity{}, fmt.Errorf("Workspace Artifact symlink %q is invalid", name)
			}
			resolved := path.Clean(path.Join(path.Dir(name), header.Linkname))
			if resolved == ".." || strings.HasPrefix(resolved, "../") {
				return TreeIdentity{}, fmt.Errorf("Workspace Artifact symlink %q escapes the root", name)
			}
			kind = treeEntrySymlink
			mode = 0o777
			payloadLength = uint64(len(header.Linkname))
		default:
			return TreeIdentity{}, fmt.Errorf("Workspace Artifact entry %q has unsupported type", name)
		}
		if _, err := digest.Write([]byte{kind}); err != nil {
			return TreeIdentity{}, err
		}
		if err := writeTreeUint32(digest, uint32(len(name))); err != nil {
			return TreeIdentity{}, err
		}
		if _, err := io.WriteString(digest, name); err != nil {
			return TreeIdentity{}, err
		}
		if err := writeTreeUint32(digest, mode); err != nil {
			return TreeIdentity{}, err
		}
		if err := writeTreeUint64(digest, payloadLength); err != nil {
			return TreeIdentity{}, err
		}
		switch kind {
		case treeEntryFile:
			if _, err := io.CopyN(digest, reader, header.Size); err != nil {
				return TreeIdentity{}, fmt.Errorf("read Workspace Artifact file %q: %w", name, err)
			}
		case treeEntrySymlink:
			if _, err := io.WriteString(digest, header.Linkname); err != nil {
				return TreeIdentity{}, err
			}
		}
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
