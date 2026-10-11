package guestd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/helmrdotdev/helmr/internal/artifact"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"google.golang.org/protobuf/proto"
)

// Startup transfer cannot execute authored code. A fresh start release follows
// materialization because artifact transfer can outlive the initial grant.
type agentProgramInput interface {
	copyArtifact(context.Context, agentv1.SessionProgramRequest_Kind, *agentv1.SessionProgramArtifact, io.Writer) error
	releaseStart(context.Context) (*agentv1.SessionGrant, error)
}

type agentProgramStore struct {
	loopMu sync.Mutex
	mu     sync.Mutex
	root   string
	images map[string]*agentProgramImage
}

type agentProgramImage struct {
	descriptor *agentv1.SessionProgramArtifact
	ready      chan struct{}
	references int
	closing    bool
	image      *mountedProgramImage
	err        error
}

type agentProgramLease struct {
	store             *agentProgramStore
	runtime, artifact *agentProgramImage
	mu                sync.Mutex
}

func validateAgentProgram(program *agentv1.SessionProgram) error {
	if program == nil {
		return errors.New("session requires its pinned Program")
	}
	for _, item := range []struct {
		descriptor *agentv1.SessionProgramArtifact
		limit      int64
	}{{program.Runtime, artifact.MaxRuntimePhysicalBytes}, {program.Artifact, artifact.MaxProgramPhysicalBytes}} {
		if item.descriptor == nil || !sha256sum.ValidDigest(item.descriptor.Digest) || item.descriptor.SizeBytes < 1 || item.descriptor.SizeBytes > item.limit {
			return errors.New("invalid Session Program artifact")
		}
	}
	return nil
}

func (s *agentProgramStore) acquire(ctx context.Context, kind agentv1.SessionProgramRequest_Kind, descriptor *agentv1.SessionProgramArtifact, input agentProgramInput) (*agentProgramImage, error) {
	key := fmt.Sprintf("%d:%s", kind, descriptor.Digest)
	s.mu.Lock()
	if s.images == nil {
		s.images = make(map[string]*agentProgramImage)
	}
	image := s.images[key]
	if image != nil {
		if image.closing || !proto.Equal(image.descriptor, descriptor) {
			s.mu.Unlock()
			return nil, errors.New("program materialization is closing or has a different descriptor")
		}
		image.references++
		s.mu.Unlock()
		select {
		case <-image.ready:
			if image.err != nil {
				return nil, errors.Join(image.err, s.release(image))
			}
			return image, nil
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), s.release(image))
		}
	}
	image = &agentProgramImage{descriptor: proto.Clone(descriptor).(*agentv1.SessionProgramArtifact), ready: make(chan struct{}), references: 1}
	s.images[key] = image
	root := s.root
	s.mu.Unlock()
	mounted, err := materializeAgentProgram(ctx, root, kind, descriptor, input, &s.loopMu)
	s.mu.Lock()
	image.image, image.err = mounted, err
	close(image.ready)
	s.mu.Unlock()
	if err != nil {
		return nil, errors.Join(err, s.release(image))
	}
	return image, nil
}

func (s *agentProgramStore) release(image *agentProgramImage) error {
	s.mu.Lock()
	if image.references > 1 {
		image.references--
		s.mu.Unlock()
		return nil
	}
	// A last reference stays owned through teardown. A failed unmount cannot
	// become an untracked file or permission for another Session to reuse it.
	image.closing = true
	s.mu.Unlock()
	var err error
	if image.image != nil {
		err = image.image.close()
	}
	if err != nil {
		return programCleanupError{err}
	}
	s.mu.Lock()
	image.references = 0
	for key, current := range s.images {
		if current == image {
			delete(s.images, key)
			break
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *agentProgramStore) materialize(ctx context.Context, program *agentv1.SessionProgram, input agentProgramInput) (*agentProgramLease, error) {
	if err := validateAgentProgram(program); err != nil {
		return nil, err
	}
	if input == nil {
		return nil, errors.New("session Program transfer is required")
	}
	lease := &agentProgramLease{store: s}
	var err error
	lease.runtime, err = s.acquire(ctx, agentv1.SessionProgramRequest_KIND_RUNTIME, program.Runtime, input)
	if err != nil {
		return nil, err
	}
	lease.artifact, err = s.acquire(ctx, agentv1.SessionProgramRequest_KIND_ARTIFACT, program.Artifact, input)
	if err != nil {
		return nil, errors.Join(err, lease.close())
	}
	return lease, nil
}

func (l *agentProgramLease) mounts() programMounts {
	return programMounts{Runtime: l.runtime.image.root, Artifact: l.artifact.image.root}
}
func (l *agentProgramLease) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var result error
	for _, reference := range []**agentProgramImage{&l.artifact, &l.runtime} {
		if *reference == nil {
			continue
		}
		if err := l.store.release(*reference); err != nil {
			result = errors.Join(result, err)
		} else {
			*reference = nil
		}
	}
	return result
}

type programCleanupError struct{ error }

func (e programCleanupError) Unwrap() error { return e.error }
