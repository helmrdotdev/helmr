package disk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/safepath"
)

// ComputerArtifact is the product-managed artifact used to seed a writable
// instance computer volume.
type ComputerArtifact struct {
	Path       string
	Digest     string
	MediaType  string
	Encoding   string
	SizeBytes  int64
	EntryCount int
}

func CreateEmptyComputerArtifact(tempDir string) (ComputerArtifact, func(), error) {
	root, err := os.MkdirTemp(tempDir, "computer-empty-")
	if err != nil {
		return ComputerArtifact{}, func() {}, fmt.Errorf("create empty computer root: %w", err)
	}
	cleanupRoot := func() { _ = os.RemoveAll(root) }
	trustedRoot := tempDir
	if strings.TrimSpace(trustedRoot) == "" {
		trustedRoot = os.TempDir()
	}
	artifact, cleanupArtifact, err := CreateComputerArtifactFromRoot(root, tempDir, trustedRoot)
	if err != nil {
		cleanupRoot()
		return ComputerArtifact{}, func() {}, err
	}
	return artifact, func() {
		cleanupArtifact()
		cleanupRoot()
	}, nil
}

func CreateComputerArtifactFromRoot(root string, tempDir string, trustedRoot string) (ComputerArtifact, func(), error) {
	return CreateComputerArtifactFromRootWithExcludes(root, tempDir, trustedRoot, nil)
}

func CreateComputerArtifactFromRootWithExcludes(root string, tempDir string, trustedRoot string, excludePatterns []string) (ComputerArtifact, func(), error) {
	return CreateComputerArtifactFromRootWithExcludesContext(context.Background(), root, tempDir, trustedRoot, excludePatterns)
}

func CreateComputerArtifactFromRootWithExcludesContext(ctx context.Context, root string, tempDir string, trustedRoot string, excludePatterns []string) (ComputerArtifact, func(), error) {
	return createComputerArtifactContext(ctx, root, tempDir, trustedRoot, excludePatterns, nil)
}

// CaptureComputerArtifactContext computes the canonical tree during archiving.
func CaptureComputerArtifactContext(ctx context.Context, root, tempDir, trustedRoot string, excludePatterns []string) (ComputerArtifact, TreeIdentity, func(), error) {
	tree := newArtifactTreeRecorder()
	artifact, cleanup, err := createComputerArtifactContext(ctx, root, tempDir, trustedRoot, excludePatterns, tree)
	if err != nil {
		return ComputerArtifact{}, TreeIdentity{}, cleanup, err
	}
	return artifact, tree.result(), cleanup, nil
}

func createComputerArtifactContext(ctx context.Context, root, tempDir, trustedRoot string, excludePatterns []string, tree *artifactTreeRecorder) (ComputerArtifact, func(), error) {
	if err := validateRootInside(root, trustedRoot); err != nil {
		return ComputerArtifact{}, func() {}, err
	}
	options := archive.TarOptions{
		ExcludePatterns: append([]string(nil), excludePatterns...),
		MaxBytes:        MaxArtifactExtractedBytes,
		MaxEntries:      MaxArtifactEntries,
	}
	if tree != nil {
		options.ObserveEntry = tree.observeHeader
	}
	tarArchive, cleanup, err := archive.CreateTarWithOptionsContext(ctx, root, tempDir, options)
	if err != nil {
		return ComputerArtifact{}, func() {}, fmt.Errorf("create computer artifact: %w", err)
	}
	return ComputerArtifact{
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
		return errors.New("trusted computer root is required")
	}
	inside, err := safepath.Contains(trustedRoot, root)
	if err != nil {
		return fmt.Errorf("resolve computer root: %w", err)
	}
	if !inside {
		return errors.New("computer root must be inside trusted root")
	}
	return nil
}
