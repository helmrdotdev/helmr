package cas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

const CheckpointVMStateMediaType = "application/vnd.helmr.checkpoint.vm-state"
const CheckpointMemoryMediaType = "application/vnd.helmr.checkpoint.memory"
const CheckpointScratchDiskMediaType = "application/vnd.helmr.checkpoint.scratch-disk"
const CheckpointManifestMediaType = "application/vnd.helmr.checkpoint.manifest"
const DeploymentSourceArtifactMediaType = "application/vnd.helmr.deployment-source.v1.tar"

const ExpirableTagKey = "helmr-expirable"
const ExpirableTagValue = "true"

type Store interface {
	Put(ctx context.Context, mediaType string, body io.Reader) (Object, error)
	Stat(ctx context.Context, digest string) (Object, error)
	Get(ctx context.Context, digest string) (io.ReadCloser, error)
	Delete(ctx context.Context, digest string) error
}

type Object struct {
	Digest    string
	SizeBytes int64
	Key       string
	MediaType string
}

func DigestBytes(bytes []byte) string {
	sum := sha256.Sum256(bytes)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func DigestReader(body io.Reader) ([]byte, string, error) {
	bytes, err := io.ReadAll(body)
	if err != nil {
		return nil, "", err
	}
	return bytes, DigestBytes(bytes), nil
}

func ObjectKey(prefix, digest string) (string, error) {
	hash, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hash) != 64 {
		return "", fmt.Errorf("unsupported digest %q", digest)
	}
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return "sha256/" + hash, nil
	}
	return prefix + "/sha256/" + hash, nil
}
