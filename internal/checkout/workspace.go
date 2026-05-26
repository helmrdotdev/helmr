package checkout

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/sourcetar"
	"github.com/helmrdotdev/helmr/internal/workspace"
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

// CreateWorkspaceArtifact packages the selected project root as a workspace
// artifact. Repository subpath is a materialization boundary, not just cwd
// metadata; callers should treat the artifact as an immutable seed for a
// writable workspace volume.
func CreateWorkspaceArtifact(worktree Worktree, tempDir string) (WorkspaceArtifact, func(), error) {
	if strings.TrimSpace(worktree.CheckoutRoot) == "" {
		return WorkspaceArtifact{}, func() {}, errors.New("workspace checkout root is required")
	}
	if strings.TrimSpace(worktree.ProjectRoot) == "" {
		return WorkspaceArtifact{}, func() {}, errors.New("workspace project root is required")
	}
	if err := validateProjectRoot(worktree); err != nil {
		return WorkspaceArtifact{}, func() {}, err
	}
	archive, cleanup, err := sourcetar.CreateTarWithOptions(worktree.ProjectRoot, tempDir, sourcetar.TarOptions{
		MaxBytes:   workspace.MaxArtifactExtractedBytes,
		MaxEntries: workspace.MaxArtifactEntries,
	})
	if err != nil {
		return WorkspaceArtifact{}, func() {}, fmt.Errorf("create workspace artifact: %w", err)
	}
	return WorkspaceArtifact{
		Path:       archive.Path,
		Digest:     archive.Digest,
		MediaType:  workspace.ArtifactMediaType,
		Encoding:   workspace.ArtifactEncoding,
		VolumeKind: workspace.VolumeKind,
		SizeBytes:  archive.SizeBytes,
		EntryCount: archive.EntryCount,
	}, cleanup, nil
}

func validateProjectRoot(worktree Worktree) error {
	rel, err := filepath.Rel(worktree.CheckoutRoot, worktree.ProjectRoot)
	if err != nil {
		return fmt.Errorf("resolve workspace project root: %w", err)
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return errors.New("workspace project root must be inside checkout root")
	}
	return nil
}
