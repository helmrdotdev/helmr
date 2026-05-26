package workspace

const (
	ArtifactMediaType = "application/vnd.helmr.workspace.v1.tar"
	ArtifactEncoding  = "tar"
	VolumeKind        = "copy-on-write"

	MaxArtifactExtractedBytes = int64(512 << 20)
	MaxArtifactEntries        = 100000
	MaxArtifactArchiveBytes   = MaxArtifactExtractedBytes + int64(MaxArtifactEntries)*2048
)
