package computer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

// GenerationCapture describes a host-owned, quiescent initial disk. The caller
// reserves MaxStagedBytes before capture and exclusively owns the unchanged Disk
// until it returns. Capture uses its seek position to skip filesystem holes.
// This is initialization only, not a running disk snapshot or a continuation.
type GenerationCapture struct {
	Disk              *os.File
	Capacity          int64
	StagingParent     string
	Scope, KeyID      string
	Key               []byte
	Fanout, PackLimit int
	MaxStagedBytes    int64
	MaxObjects        int
}

// GenerationPublication is consumed by the candidate; an execution adapter binds
// these operations to its authenticated Runtime. Upload must verify exact bytes.
type GenerationPublication interface {
	Register(context.Context, blockformat.ObjectInspection) error
	Upload(context.Context, cas.Descriptor, *os.File) (cas.Object, error)
	Certify(context.Context, blockformat.ObjectInspection) error
}

// InitialGeneration owns private staged bytes and a copy of its scoped key.
// Methods require exclusive ownership. Retry Publish on the same candidate after
// uncertainty; recapturing creates new ciphertext. Close removes only local state.
type InitialGeneration struct {
	directory    string
	store        *cas.File
	root         blockformat.Locator
	capacity     int64
	scope, keyID string
	key          []byte
	maxObjects   int
}

type generationStaging struct {
	store     *cas.File
	remaining int64
	objects   int
}

func (s *generationStaging) StoreObject(ctx context.Context, digest [32]byte, raw []byte) error {
	if s.objects <= 0 || int64(len(raw)) > s.remaining {
		return errors.New("generation staging budget exceeded")
	}
	if err := s.store.StoreObject(ctx, digest, raw); err != nil {
		return err
	}
	s.objects--
	s.remaining -= int64(len(raw))
	return nil
}

func CaptureInitialGeneration(ctx context.Context, request GenerationCapture) (_ *InitialGeneration, retErr error) {
	if request.Disk == nil || request.StagingParent == "" || request.MaxStagedBytes <= 0 || request.MaxObjects <= 0 || request.MaxObjects > 1<<20 {
		return nil, errors.New("initial generation source and finite staging admission required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := request.Disk.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != request.Capacity {
		return nil, errors.New("initial disk differs from admitted capacity")
	}
	directory, err := os.MkdirTemp(request.StagingParent, "generation-")
	if err != nil {
		return nil, err
	}
	candidate := &InitialGeneration{directory: directory, capacity: request.Capacity, scope: request.Scope, keyID: request.KeyID, key: bytes.Clone(request.Key), maxObjects: request.MaxObjects}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, candidate.Close())
		}
	}()
	candidate.store, err = cas.NewFile(filepath.Join(directory, "objects"))
	if err != nil {
		return nil, err
	}
	stage := &generationStaging{store: candidate.store, remaining: request.MaxStagedBytes, objects: request.MaxObjects}
	writer := blockformat.Writer{Source: candidate.store, Sink: stage, Scope: request.Scope, ActiveKey: request.KeyID, Keys: map[string][]byte{request.KeyID: candidate.key}, PackLimit: request.PackLimit}
	candidate.root, err = writer.Empty(ctx, request.Capacity, request.Fanout)
	if err != nil {
		return nil, err
	}
	const batchBytes = blockformat.MaxChangedBlocks * blockformat.BlockSize
	buffer := make([]byte, batchBytes)
	zero := make([]byte, blockformat.BlockSize)
	err = walkInitialDiskData(ctx, request.Disk, request.Capacity, func(offset, size int64) error {
		chunk := buffer[:size]
		if _, err = io.ReadFull(io.NewSectionReader(request.Disk, offset, size), chunk); err != nil {
			return err
		}
		changes := make(map[uint64][]byte)
		for i := 0; i < len(chunk); i += blockformat.BlockSize {
			block := chunk[i : i+blockformat.BlockSize]
			if !bytes.Equal(block, zero) {
				changes[uint64((offset+int64(i))/blockformat.BlockSize)] = block
			}
		}
		if len(changes) > 0 {
			candidate.root, err = writer.Capture(ctx, candidate.root, request.Capacity, changes)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	after, err := request.Disk.Stat()
	if err != nil {
		return nil, err
	}
	if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, errors.New("initial disk changed during capture")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return candidate, nil
}

func (c *InitialGeneration) Close() error {
	clear(c.key)
	c.key = nil
	if c.directory == "" {
		return nil
	}
	if err := os.RemoveAll(c.directory); err != nil {
		return err
	}
	c.directory = ""
	return nil
}

// Publish inspects and publishes the final physical closure child-first. Private
// intermediate roots are never registered. The returned root is object-certified,
// not a committed Computer head; the caller still owns generation publication.
func (c *InitialGeneration) Publish(ctx context.Context, publisher GenerationPublication) (blockformat.Locator, error) {
	fail := blockformat.Locator{}
	if c.directory == "" || len(c.key) != 32 {
		return fail, os.ErrClosed
	}
	if publisher == nil {
		return fail, errors.New("generation publisher required")
	}
	return publishGeneration(ctx, c.store, c.store, c.scope, map[string][]byte{c.keyID: c.key}, c.root, c.capacity, c.maxObjects, publisher, nil)
}
