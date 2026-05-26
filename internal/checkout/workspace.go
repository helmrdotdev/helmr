package checkout

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/sourcetar"
)

const (
	WorkspaceArtifactMediaType = "application/vnd.helmr.workspace.v1.tar"
	WorkspaceArtifactEncoding  = "tar"
	WorkspaceVolumeKind        = "copy-on-write"
)

// WorkspaceArtifact is the product-managed artifact used to seed a writable
// runtime workspace volume.
type WorkspaceArtifact struct {
	Path           string
	Digest         string
	MediaType      string
	Encoding       string
	VolumeKind     string
	ProjectSubpath string
}

// CreateWorkspaceArtifact packages the checked-out repository root as a
// workspace artifact. The runtime may mount or unpack this artifact, but callers
// should treat it as an immutable seed for a writable workspace volume.
func CreateWorkspaceArtifact(worktree Worktree, tempDir string) (WorkspaceArtifact, func(), error) {
	if strings.TrimSpace(worktree.CheckoutRoot) == "" {
		return WorkspaceArtifact{}, func() {}, errors.New("workspace checkout root is required")
	}
	if strings.TrimSpace(worktree.ProjectRoot) == "" {
		return WorkspaceArtifact{}, func() {}, errors.New("workspace project root is required")
	}
	projectSubpath, err := projectSubpath(worktree)
	if err != nil {
		return WorkspaceArtifact{}, func() {}, err
	}
	archive, cleanup, err := sourcetar.CreateTar(worktree.CheckoutRoot, tempDir)
	if err != nil {
		return WorkspaceArtifact{}, func() {}, fmt.Errorf("create workspace artifact: %w", err)
	}
	return WorkspaceArtifact{
		Path:           archive.Path,
		Digest:         archive.Digest,
		MediaType:      WorkspaceArtifactMediaType,
		Encoding:       WorkspaceArtifactEncoding,
		VolumeKind:     WorkspaceVolumeKind,
		ProjectSubpath: projectSubpath,
	}, cleanup, nil
}

func projectSubpath(worktree Worktree) (string, error) {
	rel, err := filepath.Rel(worktree.CheckoutRoot, worktree.ProjectRoot)
	if err != nil {
		return "", fmt.Errorf("resolve workspace project subpath: %w", err)
	}
	if rel == "." {
		return "", nil
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", errors.New("workspace project root must be inside checkout root")
	}
	return filepath.ToSlash(rel), nil
}
