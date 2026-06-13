package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/archive"
)

// WorkspaceArtifact is the product-managed artifact used to seed a writable
// runtime workspace volume.
type WorkspaceArtifact struct {
	Path       string
	Digest     string
	MediaType  string
	Encoding   string
	VolumeKind string
	SizeBytes  int64
	EntryCount int
}

func CreateEmptyWorkspaceArtifact(tempDir string) (WorkspaceArtifact, func(), error) {
	root, err := os.MkdirTemp(tempDir, "workspace-empty-")
	if err != nil {
		return WorkspaceArtifact{}, func() {}, fmt.Errorf("create empty workspace root: %w", err)
	}
	cleanupRoot := func() { _ = os.RemoveAll(root) }
	trustedRoot := tempDir
	if strings.TrimSpace(trustedRoot) == "" {
		trustedRoot = os.TempDir()
	}
	artifact, cleanupArtifact, err := createWorkspaceArtifactFromRoot(root, tempDir, trustedRoot)
	if err != nil {
		cleanupRoot()
		return WorkspaceArtifact{}, func() {}, err
	}
	return artifact, func() {
		cleanupArtifact()
		cleanupRoot()
	}, nil
}

func createWorkspaceArtifactFromRoot(root string, tempDir string, trustedRoot string) (WorkspaceArtifact, func(), error) {
	if err := validateRootInside(root, trustedRoot); err != nil {
		return WorkspaceArtifact{}, func() {}, err
	}
	tarArchive, cleanup, err := archive.CreateTarWithOptions(root, tempDir, archive.TarOptions{
		ExcludePatterns: []string{"**/.git/**"},
		MaxBytes:        MaxArtifactExtractedBytes,
		MaxEntries:      MaxArtifactEntries,
	})
	if err != nil {
		return WorkspaceArtifact{}, func() {}, fmt.Errorf("create workspace artifact: %w", err)
	}
	return WorkspaceArtifact{
		Path:       tarArchive.Path,
		Digest:     tarArchive.Digest,
		MediaType:  ArtifactMediaType,
		Encoding:   ArtifactEncoding,
		VolumeKind: VolumeKind,
		SizeBytes:  tarArchive.SizeBytes,
		EntryCount: tarArchive.EntryCount,
	}, cleanup, nil
}

func validateRootInside(root string, trustedRoot string) error {
	if strings.TrimSpace(trustedRoot) == "" {
		return errors.New("trusted workspace root is required")
	}
	rel, err := filepath.Rel(trustedRoot, root)
	if err != nil {
		return fmt.Errorf("resolve workspace root: %w", err)
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return errors.New("workspace root must be inside trusted root")
	}
	return nil
}
