package cas

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const CheckpointVMStateMediaType = "application/vnd.helmr.firecracker.vm-state.v0"
const CheckpointMemoryMediaType = "application/vnd.helmr.firecracker.memory.v0+filepack"
const CheckpointScratchDiskMediaType = "application/vnd.helmr.firecracker.scratch-disk.v0+filepack"
const CheckpointRuntimeConfigMediaType = "application/vnd.helmr.checkpoint.runtime-config.v0+json"
const ExpirableTagKey = "helmr-expirable"
const ExpirableTagValue = "true"

type Reader interface {
	Stat(ctx context.Context, digest string) (Object, error)
	Get(ctx context.Context, digest string) (io.ReadCloser, error)
}

type Store interface {
	Reader
	Put(ctx context.Context, mediaType string, body io.Reader) (Object, error)
	Stage(ctx context.Context, mediaType string) (Stage, error)
	Delete(ctx context.Context, digest string) error
}

// UploadStore admits untrusted client objects through owner-scoped quarantine
// before publishing them to the immutable digest namespace.
type UploadStore interface {
	Store
	PutQuarantine(ctx context.Context, owner string, expected Descriptor, body io.Reader) error
	PresignQuarantine(ctx context.Context, owner string, expected Descriptor, expires time.Duration) (PresignedUpload, error)
	PromoteQuarantine(ctx context.Context, owner string, expected Descriptor) (Object, error)
}

type ImmutableStore interface {
	Reader
	Publish(ctx context.Context, expected Descriptor, file *os.File) (Object, error)
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

type Descriptor struct {
	Digest    string
	SizeBytes int64
	MediaType string
}

type PresignedUpload struct {
	Method  string
	URL     string
	Headers map[string]string
}

var (
	ErrDigestMismatch = errors.New("cas object digest mismatch")
	errStageClosed    = errors.New("cas stage is closed")
	errStageCommitted = errors.New("cas stage already committed")
	errStageAborted   = errors.New("cas stage aborted")
)

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

func ShardedObjectKey(prefix, digest string) (string, error) {
	hash, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hash) != 64 {
		return "", fmt.Errorf("unsupported digest %q", digest)
	}
	prefix = strings.Trim(prefix, "/")
	key := "sha256/" + hash[:2] + "/" + hash[2:]
	if prefix == "" {
		return key, nil
	}
	return prefix + "/" + key, nil
}

func WriteStage(ctx context.Context, stage Stage, body io.Reader) (Object, error) {
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
