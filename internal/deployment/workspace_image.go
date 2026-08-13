package deployment

const (
	WorkspaceImageArtifactMediaType       = "application/vnd.helmr.workspace-image.v0.oci-tar"
	MaxWorkspaceImageBytes          int64 = 17179869184
)

// WorkspaceImage is a finalized runnable image. Build attempts, credentials,
// cache modes, and producer identity are intentionally absent.
type WorkspaceImage struct {
	DeclaredID string                 `json:"declaredId"`
	Artifact   WorkspaceImageArtifact `json:"artifact"`
}

type WorkspaceImageArtifact struct {
	Digest       string              `json:"digest"`
	SizeBytes    int64               `json:"sizeBytes"`
	MediaType    string              `json:"mediaType"`
	Architecture RuntimeArchitecture `json:"architecture"`
}
