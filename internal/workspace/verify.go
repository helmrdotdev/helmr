package workspace

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"strings"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func VerifyArtifact(body io.Reader, artifact WorkspaceArtifact, reportedTree TreeIdentity) error {
	tree, err := InspectArtifact(body, artifact)
	if err != nil {
		return err
	}
	if tree != reportedTree {
		return errors.New("workspace artifact tree does not match its receipt")
	}
	return nil
}

func InspectArtifact(body io.Reader, artifact WorkspaceArtifact) (TreeIdentity, error) {
	if artifact.Digest == "" || artifact.MediaType != ArtifactMediaType || artifact.Encoding != ArtifactEncoding ||
		artifact.SizeBytes <= 0 || artifact.SizeBytes > MaxArtifactArchiveBytes ||
		artifact.EntryCount < 0 || artifact.EntryCount > MaxArtifactEntries {
		return TreeIdentity{}, errors.New("workspace artifact descriptor is invalid")
	}
	hash := sha256.New()
	bodyLimit := &io.LimitedReader{R: io.TeeReader(body, hash), N: artifact.SizeBytes}
	tree, err := inspectArtifactTreeContext(context.Background(), bodyLimit)
	if err != nil {
		return TreeIdentity{}, err
	}
	if bodyLimit.N != 0 {
		return TreeIdentity{}, errors.New("workspace artifact ended before its declared size")
	}
	var extra [1]byte
	if n, err := io.ReadFull(body, extra[:]); n != 0 || err != io.EOF {
		return TreeIdentity{}, errors.New("workspace artifact exceeds its declared size")
	}
	if sha256sum.DigestHash(hash) != artifact.Digest {
		return TreeIdentity{}, errors.New("workspace artifact bytes do not match its digest")
	}
	if tree.EntryCount != artifact.EntryCount {
		return TreeIdentity{}, errors.New("workspace artifact entry count does not match its descriptor")
	}
	return tree, nil
}

func InspectArtifactTreeContext(ctx context.Context, path string, sizeBytes int64) (TreeIdentity, error) {
	if err := ctx.Err(); err != nil {
		return TreeIdentity{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return TreeIdentity{}, fmt.Errorf("open workspace artifact: %w", err)
	}
	defer file.Close()
	body := &io.LimitedReader{R: file, N: sizeBytes}
	tree, err := inspectArtifactTreeContext(ctx, body)
	if err != nil {
		return TreeIdentity{}, err
	}
	if body.N != 0 {
		return TreeIdentity{}, errors.New("workspace artifact ended before its declared size")
	}
	return tree, nil
}

func inspectArtifactTreeContext(ctx context.Context, body io.Reader) (TreeIdentity, error) {
	input := contextReader{ctx: ctx, reader: body}
	reader := tar.NewReader(input)
	tree := newArtifactTreeRecorder()
	for {
		if err := ctx.Err(); err != nil {
			return TreeIdentity{}, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if err := ctx.Err(); err != nil {
				return TreeIdentity{}, err
			}
			var extra [1]byte
			n, err := io.ReadFull(input, extra[:])
			if n != 0 {
				return TreeIdentity{}, errors.New("workspace artifact contains trailing bytes")
			}
			if err != io.EOF {
				return TreeIdentity{}, fmt.Errorf("finish workspace artifact: %w", err)
			}
			return tree.result(), nil
		}
		if err != nil {
			return TreeIdentity{}, fmt.Errorf("read workspace artifact: %w", err)
		}
		output, err := tree.observeHeader(header)
		if err != nil {
			return TreeIdentity{}, err
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(output, reader, header.Size); err != nil {
				return TreeIdentity{}, fmt.Errorf("read workspace artifact file %q: %w", header.Name, err)
			}
		}
	}
}

// artifactTreeRecorder is shared by archive creation and untrusted archive
// inspection, so both hash the same validated metadata and file bytes.
type artifactTreeRecorder struct {
	digest       hash.Hash
	identity     TreeIdentity
	previousPath string
	directories  map[string]struct{}
}

func newArtifactTreeRecorder() *artifactTreeRecorder {
	digest := sha256.New()
	_, _ = io.WriteString(digest, TreeDigestDomain)
	return &artifactTreeRecorder{digest: digest, directories: make(map[string]struct{})}
}

func (tree *artifactTreeRecorder) result() TreeIdentity {
	identity := tree.identity
	identity.Digest = sha256sum.DigestHash(tree.digest)
	return identity
}

func (tree *artifactTreeRecorder) observeHeader(header *tar.Header) (io.Writer, error) {
	name := header.Name
	if name == "" || strings.IndexByte(name, 0) >= 0 || path.IsAbs(name) || path.Clean(name) != name ||
		name == "." || name == ".." || strings.HasPrefix(name, "../") ||
		(tree.previousPath != "" && name <= tree.previousPath) {
		return nil, fmt.Errorf("workspace artifact path %q is invalid or out of order", name)
	}
	tree.previousPath = name
	parent := path.Dir(name)
	if parent != "." {
		if _, ok := tree.directories[parent]; !ok {
			return nil, fmt.Errorf("workspace artifact entry %q has no directory parent", name)
		}
	}
	tree.identity.EntryCount++
	if tree.identity.EntryCount > MaxArtifactEntries {
		return nil, errors.New("workspace artifact contains too many entries")
	}
	if header.Mode < 0 || header.Mode&^0o777 != 0 {
		return nil, fmt.Errorf("workspace artifact entry %q has unsupported mode", name)
	}
	mode := uint32(header.Mode)
	var kind byte
	var payloadLength uint64
	switch header.Typeflag {
	case tar.TypeDir:
		if header.Size != 0 || header.Linkname != "" {
			return nil, fmt.Errorf("workspace artifact directory %q is invalid", name)
		}
		kind = treeEntryDirectory
		tree.directories[name] = struct{}{}
	case tar.TypeReg:
		if header.Linkname != "" {
			return nil, fmt.Errorf("workspace artifact file %q is invalid", name)
		}
		if err := archive.ValidateTarRegularFileSize(header, &tree.identity.SizeBytes, MaxArtifactExtractedBytes); err != nil {
			return nil, fmt.Errorf("workspace artifact file %q is invalid: %w", name, err)
		}
		kind = treeEntryFile
		payloadLength = uint64(header.Size)
	case tar.TypeSymlink:
		if header.Size != 0 || header.Linkname == "" || strings.IndexByte(header.Linkname, 0) >= 0 || path.IsAbs(header.Linkname) {
			return nil, fmt.Errorf("workspace artifact symlink %q is invalid", name)
		}
		resolved := path.Clean(path.Join(path.Dir(name), header.Linkname))
		if resolved == ".." || strings.HasPrefix(resolved, "../") {
			return nil, fmt.Errorf("workspace artifact symlink %q escapes the root", name)
		}
		kind = treeEntrySymlink
		mode = 0o777
		payloadLength = uint64(len(header.Linkname))
	default:
		return nil, fmt.Errorf("workspace artifact entry %q has unsupported type", name)
	}
	if _, err := tree.digest.Write([]byte{kind}); err != nil {
		return nil, err
	}
	if err := writeTreeUint32(tree.digest, uint32(len(name))); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(tree.digest, name); err != nil {
		return nil, err
	}
	if err := writeTreeUint32(tree.digest, mode); err != nil {
		return nil, err
	}
	if err := writeTreeUint64(tree.digest, payloadLength); err != nil {
		return nil, err
	}
	if kind == treeEntrySymlink {
		if _, err := io.WriteString(tree.digest, header.Linkname); err != nil {
			return nil, err
		}
	}
	return tree.digest, nil
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
