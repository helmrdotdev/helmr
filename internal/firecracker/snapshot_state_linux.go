//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Maximum accepted serialized state, including its checksum. This is a restore
// format limit, not a claim that the VMM's encoder cannot produce larger output.
const snapshotStateLimit int64 = 10_000_000

// captureSnapshotState bounds writes before bytes reach the staging filesystem.
// The FIFO keeper prevents a premature EOF before the VMM opens its writer. It
// is closed after the API finishes; success requires both API and drain success.
// The caller owns stopping the paused VMM on any error or uncertain response.
func captureSnapshotState(ctx context.Context, socket, jailRoot, memoryName, stateName string, uid, gid int) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	pipePath := filepath.Join(jailRoot, stateName+".pipe")
	if err := unix.Mkfifo(pipePath, 0600); err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.Remove(pipePath)) }()
	if err := os.Chown(pipePath, uid, gid); err != nil {
		return err
	}
	fd, err := unix.Open(pipePath, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	reader := os.NewFile(uintptr(fd), pipePath)
	defer reader.Close()
	keeper, err := os.OpenFile(pipePath, os.O_WRONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer keeper.Close()
	state, err := os.OpenFile(filepath.Join(jailRoot, stateName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, state.Close()) }()
	captureCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopClosing := context.AfterFunc(captureCtx, func() { _ = reader.Close() })
	defer stopClosing()
	drained := make(chan error, 1)
	go func() {
		count, err := io.CopyN(state, reader, snapshotStateLimit)
		if errors.Is(err, io.EOF) && count > 0 {
			err = nil
		} else if err == nil {
			var extra [1]byte
			n, readErr := reader.Read(extra[:])
			if n > 0 {
				err = errors.New("snapshot VM state exceeds the supported restore limit")
			} else if readErr != io.EOF {
				err = readErr
			}
		}
		_ = reader.Close()
		if err != nil {
			cancel()
		}
		drained <- err
	}()
	apiErr := createSnapshotWithoutFileSync(captureCtx, socket, path.Join("/", memoryName), path.Join("/", stateName+".pipe"))
	keeperErr := keeper.Close()
	if apiErr != nil {
		cancel()
	}
	drainErr := <-drained
	if err := errors.Join(apiErr, keeperErr, drainErr, ctx.Err()); err != nil {
		return err
	}
	// The request disables synchronization for both files. Preserve the capture
	// boundary by synchronizing their regular files after the complete drain.
	if err := state.Sync(); err != nil {
		return fmt.Errorf("sync snapshot state: %w", err)
	}
	memory, err := os.OpenFile(filepath.Join(jailRoot, memoryName), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(memory.Sync(), memory.Close())
}

// The pinned SDK does not model sync_snapshot_files. Keep this single request
// explicit rather than silently relying on the API's default synchronization of
// a FIFO. The VMM still synchronizes its activated block devices independently.
func createSnapshotWithoutFileSync(ctx context.Context, socket, memoryPath, statePath string) error {
	body, err := json.Marshal(struct {
		SnapshotType string `json:"snapshot_type"`
		MemoryPath   string `json:"mem_file_path"`
		StatePath    string `json:"snapshot_path"`
		Sync         bool   `json:"sync_snapshot_files"`
	}{SnapshotType: CanonicalVMRuntimeDescriptor().Snapshot.CreateType, MemoryPath: memoryPath, StatePath: statePath})
	if err != nil {
		return err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost/snapshot/create", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("snapshot API returned HTTP %d", response.StatusCode)
	}
	return nil
}
