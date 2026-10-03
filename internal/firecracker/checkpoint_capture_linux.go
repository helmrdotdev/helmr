//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/firecracker-microvm/firecracker-go-sdk/client/operations"
	"github.com/helmrdotdev/helmr/internal/vm"
)

// All state is protected by the machine's computerBarrier, including Close.
// Pointer identity prevents an old capture from releasing a later hold.
type checkpointCapture struct {
	machine     *guestMachine
	request     vm.SnapshotRequest
	attempted   bool
	resumed     bool
	completed   bool
	artifact    vm.SnapshotArtifact
	delivered   bool
	snapshotErr error
}

func (s *guestMachine) BeginCheckpoint(ctx context.Context, request vm.SnapshotRequest) (vm.CheckpointCapture, error) {
	if strings.TrimSpace(request.ID) == "" {
		return nil, errors.New("checkpoint identity is required")
	}
	unlock, err := s.lockComputer(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed || s.computerHeld || s.checkpointHold != nil {
		return nil, errors.New("computer already held or closed")
	}
	capture := &checkpointCapture{machine: s, request: request}
	s.computerHeld = true
	s.checkpointHold = capture
	return capture, nil
}

func (c *checkpointCapture) CreateSnapshot(ctx context.Context) (vm.SnapshotArtifact, error) {
	s := c.machine
	unlock, err := s.lockComputer(ctx)
	if err != nil {
		return vm.SnapshotArtifact{}, err
	}
	if s.checkpointHold != c || c.attempted || c.resumed || c.completed {
		unlock()
		return vm.SnapshotArtifact{}, errors.New("checkpoint snapshot is no longer available")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		unlock()
		return vm.SnapshotArtifact{}, errors.New("computer session is closed")
	}
	// A caller timeout cannot close the snapshot FIFO while the VMM still owns
	// its writer. The capture owns this job through join; terminal Close alone
	// may cancel it, before stopping the VMM and releasing source resources.
	captureCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.computerCancel = cancel
	s.mu.Unlock()
	c.attempted = true
	done := make(chan struct{})
	go func() {
		artifact, snapshotErr := s.createCheckpointSnapshot(captureCtx, c.request)
		cancel()
		s.mu.Lock()
		c.artifact, c.snapshotErr = artifact, snapshotErr
		s.computerCancel = nil
		s.mu.Unlock()
		unlock()
		close(done)
	}()
	select {
	case <-done:
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return vm.SnapshotArtifact{}, err
		}
		if s.closed {
			return vm.SnapshotArtifact{}, errors.New("checkpoint source closed before snapshot handoff")
		}
		c.delivered = true
		return c.artifact, c.snapshotErr
	case <-ctx.Done():
		return vm.SnapshotArtifact{}, ctx.Err()
	}
}

// discardUntransferredSnapshot runs only after the snapshot job joined. The
// normal successful handoff gives these resources to the checkpoint uploader.
func (c *checkpointCapture) discardUntransferredSnapshot() error {
	c.machine.mu.Lock()
	defer c.machine.mu.Unlock()
	if c.delivered && c.snapshotErr == nil {
		return nil
	}
	if c.artifact.Computer != nil && c.artifact.Computer.Capture != nil {
		c.artifact.Computer.Capture.Release()
		c.artifact.Computer = nil
	}
	paths := []string{c.artifact.VMState.Path, c.artifact.ScratchDisk.Path}
	for _, file := range c.artifact.Memory {
		paths = append(paths, file.Path)
	}
	// Failed producers can return no artifact after creating files. Their
	// deterministic paths remain owned by this capture until removal succeeds.
	if c.attempted && c.snapshotErr != nil {
		id := safeSnapshotID(c.request.ID)
		paths = append(paths,
			filepath.Join(c.machine.jailRoot, id+snapshotMemorySuffix),
			filepath.Join(c.machine.jailRoot, id+snapshotStateSuffix),
			filepath.Join(filepath.Dir(c.machine.scratchDisk), id+snapshotScratchPackSuffix),
			filepath.Join(filepath.Dir(c.machine.scratchDisk), id+snapshotMemoryPackSuffix),
		)
	}
	var result error
	for _, path := range paths {
		if path != "" {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				result = errors.Join(result, err)
			}
		}
	}
	return result
}

func (c *checkpointCapture) ResumeGuestControl(ctx context.Context) error {
	s := c.machine
	unlock, err := s.lockComputer(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed || s.checkpointHold != c || c.completed {
		return errors.New("checkpoint no longer owns the source")
	}
	if c.resumed {
		return nil
	}
	if err := c.discardUntransferredSnapshot(); err != nil {
		return err
	}
	if !c.attempted {
		// A failed guest freeze can abort before the VMM was ever paused.
		c.resumed = true
		return nil
	}
	// Keep the dispatch hold after a lost response. Repeating the VMM operation
	// does not grant user execution; matching member holds remain in the guest.
	if err := s.machine.ResumeVM(ctx, func(p *operations.PatchVMParams) { p.SetContext(ctx) }); err != nil {
		return fmt.Errorf("resume checkpoint guest control: %w", err)
	}
	c.resumed = true
	return nil
}

func (c *checkpointCapture) CompleteAbort(ctx context.Context) error {
	s := c.machine
	unlock, err := s.lockComputer(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if c.completed {
		return nil
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed || s.checkpointHold != c || !c.resumed {
		return errors.New("checkpoint guest control has not resumed")
	}
	c.completed = true
	s.checkpointHold = nil
	s.computerHeld = false
	return nil
}
