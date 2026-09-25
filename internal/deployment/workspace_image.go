package deployment

import (
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/oci"
)

const (
	WorkspaceImageArtifactMediaType       = computer.SeedMediaType
	MaxWorkspaceImageBytes          int64 = 17179869184
)

// WorkspaceImage is a finalized runnable image. Build attempts, credentials,
// cache modes, and producer identity are intentionally absent.
type WorkspaceImage struct {
	DeclaredID string                 `json:"declaredId"`
	Artifact   WorkspaceImageArtifact `json:"artifact"`
}

type WorkspaceImageArtifact struct {
	Profile      string              `json:"profile"`
	Config       oci.RuntimeConfig   `json:"config"`
	Digest       string              `json:"digest"`
	SizeBytes    int64               `json:"sizeBytes"`
	MediaType    string              `json:"mediaType"`
	Architecture RuntimeArchitecture `json:"architecture"`
}
