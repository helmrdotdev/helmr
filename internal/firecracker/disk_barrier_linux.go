//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/vm"
)

// PauseComputer establishes a fresh API dispatch hold, even for a restored VM.
// The owner must stop the source on any error or ambiguous response.
func (s *guestSession) PauseComputer(ctx context.Context) (*vm.RuntimeComputer, error) {
	if err := s.machine.PauseVM(ctx); err != nil {
		return nil, fmt.Errorf("pause Firecracker vm: %w", err)
	}
	if err := s.syncPausedDisks(ctx); err != nil {
		return nil, err
	}
	return cloneRuntimeComputer(s.topology.Computer), nil
}

// syncPausedDisks requires an acknowledged API Pause, whose dispatch hold must
// remain owned through snapshot serialization and disk capture. Synchronous
// engines leave no submitted asynchronous device I/O outside that hold. Validate
// the actual device configuration, including restored devices, before relying on
// it. Snapshot API success does not report backing-file synchronization errors.
func (s *guestSession) syncPausedDisks(ctx context.Context) error {
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
		if *drive.ReadOnly || strings.TrimPrefix(drive.Path, "/") != filepath.Base(path) {
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
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(errors.New("paused backing file must be regular"), err)
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
		if err != nil || !info.Mode().IsRegular() {
			return files, errors.Join(errors.New("runtime backing file must be regular"), err)
		}
	}
	return files, nil
}

func closeRuntimeDiskFiles(files map[string]*os.File) error {
	var result error
	for _, id := range []string{"computer", scratchDriveID} {
		if file := files[id]; file != nil {
			result = errors.Join(result, file.Close())
		}
	}
	return result
}
