package frameio

import (
	"crypto/sha256"
	"io"
	"os"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func WriteFileFrameWithMetadata(w io.Writer, headerBytes []byte, path string, size int64) error {
	if err := WriteStreamFrameHeader(w, headerBytes, uint64(size)); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(w, file)
	return err
}

func HashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return sha256sum.DigestHash(hash), size, nil
}
