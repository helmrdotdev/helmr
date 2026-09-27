package deployment

import (
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/oci"
)

const (
	ComputerImageArtifactMediaType       = computer.SeedMediaType
	MaxComputerImageBytes          int64 = 17179869184
)

// ComputerImage is a finalized runnable image. Build attempts, credentials,
// cache modes, and producer identity are intentionally absent.
type ComputerImage struct {
	DeclaredID string                `json:"declaredId"`
	Artifact   ComputerImageArtifact `json:"artifact"`
}

type ComputerImageArtifact struct {
	Profile      string              `json:"profile"`
	Config       oci.RuntimeConfig   `json:"config"`
	Digest       string              `json:"digest"`
	SizeBytes    int64               `json:"sizeBytes"`
	MediaType    string              `json:"mediaType"`
	Architecture RuntimeArchitecture `json:"architecture"`
}
