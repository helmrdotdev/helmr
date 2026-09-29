package disk

const (
	ArtifactMediaType = "application/vnd.helmr.computer.v0.tar"
	ArtifactEncoding  = "tar"

	MaxArtifactExtractedBytes = int64(512 << 20)
	MaxArtifactEntries        = 100000
	MaxArtifactArchiveBytes   = MaxArtifactExtractedBytes + int64(MaxArtifactEntries)*2048
)
