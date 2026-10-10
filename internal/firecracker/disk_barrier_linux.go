//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"uuid"

	"github.com/firecracker-microvm/firecracker-go-sdk/client/operations"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
)

// computerCaptureTimeout bounds guest writeback, host drain, disk encoding and
// root persistence. Checkpoint memory serialization uses its caller budget.
const computerCaptureTimeout = 30 * time.Second

// lockComputer serializes live disk cuts with checkpoint and terminal close
// holds. Waiting is cancellable; a timed-out Close can be retried.
func (s *guestMachine) lockComputer(ctx context.Context) (func(), error) {
	s.mu.Lock()
	if s.computerBarrier == nil {
		s.computerBarrier = make(chan struct{}, 1)
	}
	gate := s.computerBarrier
	s.mu.Unlock()
	select {
	case gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-gate
			return nil, err
		}
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// captureContext lets Close cancel and join a disk operation before releasing
// its device or backing descriptors. The caller owns computerBarrier. The finite
// capture budget includes synchronous disk I/O and guest acknowledgement.
func (s *guestMachine) captureContext(ctx context.Context) (context.Context, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, errors.New("computer machine is closed")
	}
	ctx, cancel := context.WithTimeout(ctx, computerCaptureTimeout)
	s.computerCancel = cancel
	return ctx, func() {
		cancel()
		s.mu.Lock()
		s.computerCancel = nil
		s.mu.Unlock()
	}, nil
}

// PauseComputerForTermination establishes an irreversible dispatch hold for terminal
// capture. It revokes any reversible checkpoint handle; the source must stop.
func (s *guestMachine) PauseComputerForTermination(ctx context.Context) (*vm.ComputerSnapshot, error) {
	unlock, err := s.lockComputer(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.checkpointHold = nil
	s.computerCaptureBlocked = true
	ctx, done, err := s.captureContext(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	return s.capturePausedComputer(ctx)
}

// computerhost.PreparedMachines admits only machines that can capture a live
// Computer for saves.
var _ interface {
	CaptureComputer(context.Context) (*vm.ComputerSnapshot, error)
} = (*guestMachine)(nil)

// CaptureComputer flushes the retained guest filesystem and takes an ordered
// online disk cut. It does not pause the VM, processes or filesystem. Any error
// fences further captures until the owner stops the unsafe source.
func (s *guestMachine) CaptureComputer(ctx context.Context) (*vm.ComputerSnapshot, error) {
	unlock, err := s.lockComputer(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if s.computerCaptureBlocked {
		return nil, errors.New("computer capture is fenced")
	}
	s.computerCaptureBlocked = true
	ctx, done, err := s.captureContext(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	if s.topology.Computer == nil || s.topology.Computer.Device == nil {
		return nil, errors.New("owned Computer device required")
	}
	started := time.Now()
	if err := s.flushGuestComputer(ctx); err != nil {
		return nil, err
	}
	slog.Info("Computer guest flush completed", "computer_id", s.topology.Computer.ComputerID, "duration_ms", float64(time.Since(started))/float64(time.Millisecond))
	started = time.Now()
	capture, err := s.topology.Computer.Device.Capture(ctx)
	if err != nil {
		return nil, err
	}
	// Includes device drainage, the request boundary, encoding and local
	// persistence; it is not a measurement of cut synchronization alone.
	slog.Info("Computer online capture completed", "computer_id", s.topology.Computer.ComputerID, "duration_ms", float64(time.Since(started))/float64(time.Millisecond))
	s.computerCaptureBlocked = false
	return &vm.ComputerSnapshot{ComputerID: s.topology.Computer.ComputerID, Capture: capture}, nil
}

func (s *guestMachine) flushGuestComputer(ctx context.Context) error {
	stream, err := s.OpenStream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(closed); _ = stream.Close() })
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	operation := uuid.NewV7().String()
	if err = wire.WriteStreamFrameHeader(stream, wire.StreamHeader{Type: wire.StreamTypeComputerFlush, ComputerID: s.topology.Computer.ComputerID, OperationID: operation}, 0); err != nil {
		return errors.Join(ctx.Err(), err)
	}
	if err = frameio.WriteProtoFrame(stream, &computerv0.FlushComputerRequest{OperationId: operation}); err != nil {
		return errors.Join(ctx.Err(), err)
	}
	var response computerv0.FlushComputerResponse
	if err = frameio.ReadProtoFrameBounded(stream, 4096, &response); err != nil {
		return errors.Join(ctx.Err(), err)
	}
	if response.OperationId != operation || response.Error != "" {
		return errors.New("guest Computer writeback was not acknowledged")
	}
	return ctx.Err()
}

func (s *guestMachine) capturePausedComputer(ctx context.Context) (*vm.ComputerSnapshot, error) {
	if err := s.machine.PauseVM(ctx, func(params *operations.PatchVMParams) { params.SetContext(ctx) }); err != nil {
		return nil, fmt.Errorf("pause Firecracker vm: %w", err)
	}
	if err := s.syncPausedDisks(ctx); err != nil {
		return nil, err
	}
	if s.topology.Computer.Device == nil {
		return nil, errors.New("owned disk version device required for capture")
	}
	capture, err := s.topology.Computer.Device.Capture(ctx)
	if err != nil {
		return nil, err
	}
	return &vm.ComputerSnapshot{ComputerID: s.topology.Computer.ComputerID, Capture: capture}, nil
}

// syncPausedDisks requires an acknowledged API Pause, whose dispatch hold must
// remain owned through snapshot serialization and disk capture. Synchronous
// engines leave no submitted asynchronous device I/O outside that hold. Validate
// the actual device configuration, including restored devices, before relying on
// it. Snapshot API success does not report backing-file synchronization errors.
func (s *guestMachine) syncPausedDisks(ctx context.Context) error {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", s.machine.Cfg.SocketPath)
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/vm/config", nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return fmt.Errorf("inspect paused device configuration: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("device configuration API returned HTTP %d", response.StatusCode)
	}
	const limit = 64 << 10
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(raw) > limit {
		return errors.Join(errors.New("read bounded device configuration"), err)
	}
	var config struct {
		Drives []struct {
			ID       string `json:"drive_id"`
			Cache    string `json:"cache_type"`
			Engine   string `json:"io_engine"`
			ReadOnly *bool  `json:"is_read_only"`
			Path     string `json:"path_on_host"`
		} `json:"drives"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return fmt.Errorf("decode device configuration: %w", err)
	}
	if s.topology.Computer == nil {
		return errors.New("paused Computer disk is missing")
	}
	expected := map[string]string{"computer": s.topology.Computer.Path, scratchDriveID: s.scratchDisk}
	seen := make(map[string]bool)
	backings := make(map[string]string)
	for _, drive := range config.Drives {
		if drive.ID == "" || seen[drive.ID] || drive.Engine != blockIOEngine || drive.ReadOnly == nil {
			return errors.New("paused devices require distinct identities and synchronous I/O")
		}
		seen[drive.ID] = true
		path, writable := expected[drive.ID]
		if !writable {
			if !*drive.ReadOnly {
				return errors.New("unexpected writable paused device")
			}
			continue
		}
		// API paths belong to the VMM jail, not the Worker's host root.
		actual := filepath.Join(s.jailRoot, strings.TrimPrefix(drive.Path, "/"))
		if drive.Cache != writableBlockCache {
			return fmt.Errorf("paused %s device requires writeback cache", drive.ID)
		}
		jailedName := filepath.Base(path)
		if drive.ID == scratchDriveID {
			// Restored scratch has a unique host filename, while the snapshot
			// and withJailedRestoreFiles retain the canonical jailed name.
			jailedName = scratchDiskName
		}
		if *drive.ReadOnly || strings.TrimPrefix(drive.Path, "/") != jailedName {
			return fmt.Errorf("paused %s device does not match its owned backing file", drive.ID)
		}
		backings[drive.ID] = actual
	}
	for _, id := range []string{"computer", scratchDriveID} {
		if !seen[id] {
			return fmt.Errorf("paused %s device is missing", id)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syncPausedBacking(s.diskFiles[id], expected[id], backings[id]); err != nil {
			return fmt.Errorf("sync paused %s backing file: %w", id, err)
		}
	}
	return nil
}

// The descriptor predates guest execution so its writeback error cursor cannot
// skip a failure already observed by another file description in the VMM.
func syncPausedBacking(file *os.File, source, backing string) error {
	if file == nil {
		return errors.New("retained backing descriptor is missing")
	}
	info, err := file.Stat()
	if err != nil || !validComputerBacking(info) {
		return errors.Join(errors.New("paused backing must be a regular file or block device"), err)
	}
	for _, path := range []string{source, backing} {
		linked, err := os.Stat(path)
		if err != nil || !os.SameFile(info, linked) {
			return errors.Join(errors.New("paused device backing inode changed"), err)
		}
	}
	return file.Sync()
}

func openRuntimeDiskFiles(scratch, computer string) (files map[string]*os.File, retErr error) {
	files = make(map[string]*os.File)
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, closeRuntimeDiskFiles(files))
		}
	}()
	for _, disk := range []struct{ id, path string }{{scratchDriveID, scratch}, {"computer", computer}} {
		if disk.id == "computer" && disk.path == "" {
			continue
		}
		file, err := os.OpenFile(disk.path, os.O_RDWR, 0)
		if err != nil {
			return files, fmt.Errorf("retain %s backing descriptor: %w", disk.id, err)
		}
		files[disk.id] = file
		info, err := file.Stat()
		if err != nil {
			return files, err
		}
		if disk.id == "computer" && !validComputerBacking(info) || disk.id != "computer" && !info.Mode().IsRegular() {
			return files, errors.New("invalid runtime backing type")
		}
	}
	return files, nil
}

func closeRuntimeDiskFiles(files map[string]*os.File) error {
	var result error
	for _, id := range []string{"computer", scratchDriveID} {
		if file := files[id]; file != nil {
			if err := file.Close(); !errors.Is(err, os.ErrClosed) {
				result = errors.Join(result, err)
			}
		}
	}
	return result
}

// WithRunningGuestControl holds the same barrier as snapshot/termination. The
// caller's exchange may not recursively invoke a VM lifecycle operation.
func (s *guestMachine) WithRunningGuestControl(ctx context.Context, stage vm.GuestControlStage, exchange func(context.Context) error) error {
	unlock, err := s.lockComputer(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if (s.checkpointHold != nil && !s.checkpointHold.resumed) || (s.computerCaptureBlocked && s.checkpointHold == nil) {
		return errors.New("computer guest control is paused or fenced")
	}
	ctx, done, err := s.captureContext(ctx)
	if err != nil {
		return err
	}
	defer done()
	if stage == vm.GuestControlInstallation {
		s.agentContinuationPending = true
	}
	err = exchange(ctx)
	if err == nil && stage == vm.GuestControlActivation {
		s.agentContinuationPending = false
	}
	return err
}
