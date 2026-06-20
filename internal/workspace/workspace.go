package workspace

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/safepath"
)

// WorkspaceArtifact is the product-managed artifact used to seed a writable
// runtime workspace volume.
type WorkspaceArtifact struct {
	Path       string
	Digest     string
	MediaType  string
	Encoding   string
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
	artifact, cleanupArtifact, err := CreateWorkspaceArtifactFromRoot(root, tempDir, trustedRoot)
	if err != nil {
		cleanupRoot()
		return WorkspaceArtifact{}, func() {}, err
	}
	return artifact, func() {
		cleanupArtifact()
		cleanupRoot()
	}, nil
}

func CreateWorkspaceArtifactFromRoot(root string, tempDir string, trustedRoot string) (WorkspaceArtifact, func(), error) {
	return CreateWorkspaceArtifactFromRootWithExcludes(root, tempDir, trustedRoot, nil)
}

func CreateWorkspaceArtifactFromRootWithExcludes(root string, tempDir string, trustedRoot string, excludePatterns []string) (WorkspaceArtifact, func(), error) {
	if err := validateRootInside(root, trustedRoot); err != nil {
		return WorkspaceArtifact{}, func() {}, err
	}
	excludes := append([]string{"**/.git/**"}, excludePatterns...)
	tarArchive, cleanup, err := archive.CreateTarWithOptions(root, tempDir, archive.TarOptions{
		ExcludePatterns: excludes,
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
		SizeBytes:  tarArchive.SizeBytes,
		EntryCount: tarArchive.EntryCount,
	}, cleanup, nil
}

func validateRootInside(root string, trustedRoot string) error {
	if strings.TrimSpace(trustedRoot) == "" {
		return errors.New("trusted workspace root is required")
	}
	inside, err := safepath.Contains(trustedRoot, root)
	if err != nil {
		return fmt.Errorf("resolve workspace root: %w", err)
	}
	if !inside {
		return errors.New("workspace root must be inside trusted root")
	}
	return nil
}
