package deployment

import "fmt"

type ArtifactDescriptor struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType"`
	SizeBytes int64  `json:"sizeBytes"`
}

func validateInputArtifact(
	artifact ArtifactDescriptor,
	mediaType string,
	maxSize int64,
	label string,
) error {
	if !sha256DigestPattern.MatchString(artifact.Digest) {
		return fmt.Errorf("%s digest is not a lowercase SHA-256 digest", label)
	}
	if artifact.MediaType != mediaType {
		return fmt.Errorf(
			"%s mediaType = %q, want %q",
			label,
			artifact.MediaType,
			mediaType,
		)
	}
	if artifact.SizeBytes < 1 || artifact.SizeBytes > maxSize {
		return fmt.Errorf(
			"%s sizeBytes is outside [1,%d]",
			label,
			maxSize,
		)
	}
	return nil
}
