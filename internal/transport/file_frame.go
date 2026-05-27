package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

func WriteFileFrame(w io.Writer, header StreamHeader, path string) error {
	hash, size, err := HashFile(path)
	if err != nil {
		return err
	}
	return WriteFileFrameWithMetadata(w, header, path, hash, size)
}

func WriteFileFrameWithMetadata(w io.Writer, header StreamHeader, path string, digest string, size int64) error {
	header.BodyDigest = &digest
	if err := WriteStreamFrameHeader(w, header, uint64(size)); err != nil {
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
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), size, nil
}
