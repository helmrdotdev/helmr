package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestInspectArtifactTreeHonorsCancelledContext(t *testing.T) {
	trustedRoot := t.TempDir()
	root := filepath.Join(trustedRoot, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact, cleanup, err := CreateWorkspaceArtifactFromRoot(root, t.TempDir(), trustedRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = InspectArtifactTreeContext(ctx, artifact.Path, artifact.SizeBytes)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("inspect cancelled Workspace Artifact error = %v", err)
	}
}

func TestVerifyArtifactRecomputesCanonicalTreeAndPackaging(t *testing.T) {
	trustedRoot := t.TempDir()
	root := filepath.Join(trustedRoot, "tree")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "result.txt"), []byte("result"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("result.txt", filepath.Join(root, "latest")); err != nil {
		t.Fatal(err)
	}
	tree, err := InspectTree(root)
	if err != nil {
		t.Fatal(err)
	}
	artifact, cleanup, err := CreateWorkspaceArtifactFromRoot(root, trustedRoot, trustedRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	file, err := os.Open(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifact(file, artifact, tree); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	file, err = os.Open(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	wrongTree := tree
	wrongTree.Digest = CanonicalEmptyTreeDigest
	if err := VerifyArtifact(file, artifact, wrongTree); err == nil {
		_ = file.Close()
		t.Fatal("mismatched reported tree was accepted")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyArtifactReadsModeZeroEntriesWithoutMaterializingThem(t *testing.T) {
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	if err := writer.WriteHeader(&tar.Header{Name: "locked", Typeflag: tar.TypeDir, Mode: 0}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "locked/file", Typeflag: tar.TypeReg, Mode: 0, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(TreeDigestDomain))
	_, _ = digest.Write([]byte{treeEntryDirectory})
	_ = writeTreeUint32(digest, uint32(len("locked")))
	_, _ = digest.Write([]byte("locked"))
	_ = writeTreeUint32(digest, 0)
	_ = writeTreeUint64(digest, 0)
	_, _ = digest.Write([]byte{treeEntryFile})
	_ = writeTreeUint32(digest, uint32(len("locked/file")))
	_, _ = digest.Write([]byte("locked/file"))
	_ = writeTreeUint32(digest, 0)
	_ = writeTreeUint64(digest, 1)
	_, _ = digest.Write([]byte("x"))
	tree := TreeIdentity{Digest: sha256sum.DigestHash(digest), SizeBytes: 1, EntryCount: 2}
	artifact := WorkspaceArtifact{
		Digest: sha256sum.DigestBytes(body.Bytes()), MediaType: ArtifactMediaType,
		Encoding: ArtifactEncoding, SizeBytes: int64(body.Len()), EntryCount: 2,
	}
	if err := VerifyArtifact(bytes.NewReader(body.Bytes()), artifact, tree); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyArtifactRequiresExplicitDirectoryParents(t *testing.T) {
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	if err := writer.WriteHeader(&tar.Header{Name: "missing/file", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	artifact := WorkspaceArtifact{
		Digest: sha256sum.DigestBytes(body.Bytes()), MediaType: ArtifactMediaType,
		Encoding: ArtifactEncoding, SizeBytes: int64(body.Len()), EntryCount: 1,
	}
	if err := VerifyArtifact(bytes.NewReader(body.Bytes()), artifact, TreeIdentity{}); err == nil {
		t.Fatal("Artifact with an implicit directory parent was accepted")
	}
}

func TestVerifyArtifactRejectsSparseMetadata(t *testing.T) {
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	header := &tar.Header{
		Name: "sparse.bin", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1,
		PAXRecords: map[string]string{"GNU.sparse.realsize": "1099511627776"},
	}
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	artifact := WorkspaceArtifact{
		Digest: sha256sum.DigestBytes(body.Bytes()), MediaType: ArtifactMediaType,
		Encoding: ArtifactEncoding, SizeBytes: int64(body.Len()), EntryCount: 1,
	}
	if err := VerifyArtifact(bytes.NewReader(body.Bytes()), artifact, TreeIdentity{}); err == nil {
		t.Fatal("sparse Artifact was accepted")
	}
}
