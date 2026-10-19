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
	"path/filepath"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/helmrdotdev/helmr/internal/vm"
)

// Only snapshot loading differs from the pinned SDK. Jailer, network, devices,
// process ownership and the remaining initialization handlers stay unchanged.
func withLazyMemoryRestore(source vm.MemorySource, lifetime context.Context, stopVMM context.CancelFunc, pager **memoryPager) firecracker.Opt {
	return func(machine *firecracker.Machine) {
		machine.Handlers.Validation = machine.Handlers.Validation.Swap(firecracker.Handler{Name: firecracker.ValidateLoadSnapshotCfgHandlerName, Fn: func(_ context.Context, m *firecracker.Machine) error {
			cfg := m.Cfg
			// The SDK stats a File backend path. Validate all its other contracts with
			// the already-required state file, without inventing a memory backing.
			cfg.Snapshot.MemFilePath = cfg.Snapshot.SnapshotPath
			return cfg.ValidateLoadSnapshot()
		}})
		machine.Handlers.FcInit = machine.Handlers.FcInit.Swap(firecracker.Handler{Name: firecracker.LoadSnapshotHandlerName, Fn: func(ctx context.Context, m *firecracker.Machine) error {
			ctx, cancelLoad := context.WithTimeout(ctx, 30*time.Second)
			defer cancelLoad()
			root := jailRootPath(Config{FirecrackerPath: m.Cfg.JailerCfg.ExecFile, JailerChrootBaseDir: m.Cfg.JailerCfg.ChrootBaseDir}, m.Cfg.JailerCfg.ID)
			socket := filepath.Join(root, "memory.sock")
			listener, err := listenMemoryPager(socket, *m.Cfg.JailerCfg.UID, *m.Cfg.JailerCfg.GID)
			if err != nil {
				return err
			}
			defer listener.Close()
			pid, err := m.PID()
			if err != nil {
				return err
			}
			handshakeCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			type accepted struct {
				pager *memoryPager
				err   error
			}
			result := make(chan accepted, 1)
			go func() {
				fd, regions, err := receiveMemoryHandshake(handshakeCtx, listener, pid, source.Size())
				if err != nil {
					stopVMM()
					result <- accepted{err: err}
					return
				}
				// Closing unlinks the listener. The accepted connection has already been
				// closed and only this physical VMM can receive page copies through its fd.
				_ = listener.Close()
				m.Logger().Printf("memory pager attached instance_id=%s pid=%d memory_bytes=%d", m.Cfg.JailerCfg.ID, pid, source.Size())
				result <- accepted{pager: newMemoryPager(lifetime, fd, regions, source, stopVMM)}
			}()
			payload := struct {
				SnapshotPath string            `json:"snapshot_path"`
				MemBackend   map[string]string `json:"mem_backend"`
				EnableDiff   bool              `json:"enable_diff_snapshots"`
				Resume       bool              `json:"resume_vm"`
			}{SnapshotPath: m.Cfg.Snapshot.SnapshotPath, MemBackend: map[string]string{"backend_type": "Uffd", "backend_path": "/memory.sock"}}
			raw, err := json.Marshal(payload)
			if err == nil {
				err = loadMemorySnapshot(ctx, m.Cfg.SocketPath, raw)
			}
			if err != nil {
				cancel()
				_ = listener.Close()
			}
			handshake := <-result
			*pager = handshake.pager
			if err = errors.Join(err, handshake.err, handshake.pager.failure()); err != nil {
				return err
			}
			return nil
		}})
	}
}
func loadMemorySnapshot(ctx context.Context, socket string, body []byte) error {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://firecracker/snapshot/load", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return err
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	closeErr := response.Body.Close()
	if err = errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("load Firecracker memory snapshot: status %d: %s", response.StatusCode, raw)
	}
	return nil
}
func materializeEagerMemory(ctx context.Context, path string, source vm.MemorySource) (resultErr error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
		if resultErr != nil {
			_ = os.Remove(path)
		}
	}()
	buffer := make([]byte, vm.MemoryReadBytes)
	for offset := int64(0); offset < source.Size(); offset += int64(len(buffer)) {
		data := buffer[:min(int64(len(buffer)), source.Size()-offset)]
		n, err := source.ReadAt(ctx, data, offset)
		if err != nil {
			return err
		}
		if n != len(data) {
			return errors.New("incomplete eager memory read")
		}
		if _, err = file.Write(data); err != nil {
			return err
		}
	}
	return nil
}
