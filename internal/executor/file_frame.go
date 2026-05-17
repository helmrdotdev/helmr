package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	"github.com/helmrdotdev/helmr/internal/guest"
)

func writeFileFrame(w io.Writer, header guest.StreamHeader, path string) error {
	hash, size, err := hashFile(path)
	if err != nil {
		return err
	}
	header.ContentHash = &hash
	if err := guest.WriteStreamFrameHeader(w, header, uint64(size)); err != nil {
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

func hashFile(path string) (string, int64, error) {
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
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), size, nil
}
