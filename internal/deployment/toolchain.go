package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	ToolchainMediaType = "application/vnd.helmr.standard-toolchain.v0+squashfs"

	maxToolArtifactBytes int64 = 4 << 30
)

func SHA256DigestBytes(value string) ([]byte, error) {
	if !validToolDigest(value) {
		return nil, errors.New(
			"digest is not a lowercase SHA-256 digest",
		)
	}
	decoded, err := hex.DecodeString(value[len("sha256:"):])
	if err != nil {
		return nil, errors.New(
			"digest is not a lowercase SHA-256 digest",
		)
	}
	return decoded, nil
}

func SHA256DigestString(value []byte) (string, error) {
	if len(value) != sha256.Size {
		return "", fmt.Errorf("digest is not %d bytes", sha256.Size)
	}
	return "sha256:" + hex.EncodeToString(value), nil
}

func validToolDigest(value string) bool {
	return sha256DigestPattern.MatchString(value)
}
