package cas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

const CheckpointVMStateMediaType = "application/vnd.helmr.checkpoint.vm-state"
const CheckpointMemoryMediaType = "application/vnd.helmr.firecracker.memory.v1+filepack"
const CheckpointScratchDiskMediaType = "application/vnd.helmr.firecracker.scratch-disk.v1+filepack"
const CheckpointRuntimeConfigMediaType = "application/vnd.helmr.checkpoint.runtime-config"
const DeploymentSourceArtifactMediaType = "application/vnd.helmr.deployment-source.v1.tar"

const ExpirableTagKey = "helmr-expirable"
const ExpirableTagValue = "true"

type Store interface {
	Put(ctx context.Context, mediaType string, body io.Reader) (Object, error)
	Stage(ctx context.Context, mediaType string) (Stage, error)
	Stat(ctx context.Context, digest string) (Object, error)
	Get(ctx context.Context, digest string) (io.ReadCloser, error)
	Delete(ctx context.Context, digest string) error
}

// Stage receives object bytes, hashes and counts them, then publishes on Commit.
type Stage interface {
	io.WriteCloser
	Commit(ctx context.Context) (Object, error)
	Abort(ctx context.Context) error
}

type Object struct {
	Digest    string
	SizeBytes int64
	Key       string
	MediaType string
}

var (
	errStageClosed    = errors.New("cas stage is closed")
	errStageCommitted = errors.New("cas stage already committed")
	errStageAborted   = errors.New("cas stage aborted")
)

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

func putStage(ctx context.Context, stage Stage, body io.Reader) (Object, error) {
	if _, err := io.Copy(stage, body); err != nil {
		_ = stage.Abort(context.Background())
		return Object{}, err
	}
	object, err := stage.Commit(ctx)
	if err != nil {
		_ = stage.Abort(context.Background())
		return Object{}, err
	}
	return object, nil
}
