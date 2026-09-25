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
	"testing"
	"time"
)

func serveSnapshotAPI(t *testing.T, root string, handler http.HandlerFunc) string {
	t.Helper()
	socket := filepath.Join(root, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	done := make(chan struct{})
	go func() { _ = server.Serve(listener); close(done) }()
	t.Cleanup(func() { _ = server.Close(); <-done })
	return socket
}

func TestBoundedSnapshotStateCapture(t *testing.T) {
	for _, size := range []int{0, 1, int(snapshotStateLimit), int(snapshotStateLimit) + 1, int(snapshotStateLimit) * 2} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			root := t.TempDir()
			socket := serveSnapshotAPI(t, root, func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Type   string `json:"snapshot_type"`
					State  string `json:"snapshot_path"`
					Memory string `json:"mem_file_path"`
					Sync   *bool  `json:"sync_snapshot_files"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if r.Method != http.MethodPut || r.URL.Path != "/snapshot/create" || request.Type != "Full" || request.Sync == nil || *request.Sync {
					t.Error("incorrect snapshot API contract")
					w.WriteHeader(400)
					return
				}
				state, err := os.OpenFile(filepath.Join(root, filepath.Base(request.State)), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
				if err == nil {
					_, err = io.Copy(state, bytes.NewReader(bytes.Repeat([]byte{42}, size)))
					err = errors.Join(err, state.Close())
				}
				if err == nil {
					err = os.WriteFile(filepath.Join(root, filepath.Base(request.Memory)), []byte("memory"), 0600)
				}
				if err != nil {
					w.WriteHeader(500)
					return
				}
				w.WriteHeader(204)
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := captureSnapshotState(ctx, socket, root, "memory", "state", os.Getuid(), os.Getgid())
			info, statErr := os.Stat(filepath.Join(root, "state"))
			if statErr != nil {
				t.Fatal(statErr)
			}
			if size > 0 && size <= int(snapshotStateLimit) {
				if err != nil || info.Size() != int64(size) {
					t.Fatalf("valid size=%d stored=%d err=%v", size, info.Size(), err)
				}
			} else if err == nil || info.Size() > snapshotStateLimit {
				t.Fatalf("overflow size=%d stored=%d err=%v", size, info.Size(), err)
			}
			if _, err := os.Stat(filepath.Join(root, "state.pipe")); !os.IsNotExist(err) {
				t.Fatalf("FIFO survived capture: %v", err)
			}
		})
	}
}

func TestBoundedSnapshotStateCancellationBeforeWriter(t *testing.T) {
	root := t.TempDir()
	entered := make(chan struct{})
	socket := serveSnapshotAPI(t, root, func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
		}
		close(entered)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- captureSnapshotState(ctx, socket, root, "memory", "state", os.Getuid(), os.Getgid()) }()
	<-entered
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("capture did not join drain on cancellation")
	}
}

func TestBoundedSnapshotStateRejectsAPIFailureWithoutWriter(t *testing.T) {
	root := t.TempDir()
	socket := serveSnapshotAPI(t, root, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := captureSnapshotState(ctx, socket, root, "memory", "state", os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("API failure accepted")
	}
}

func TestBoundedSnapshotStateRejectsPartialAPIFailure(t *testing.T) {
	root := t.TempDir()
	socket := serveSnapshotAPI(t, root, func(w http.ResponseWriter, r *http.Request) {
		state, err := os.OpenFile(filepath.Join(root, "state.pipe"), os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		_, err = state.Write([]byte("incomplete state"))
		if err != nil {
			t.Error(err)
		}
		if err := state.Close(); err != nil {
			t.Error(err)
		}
		w.WriteHeader(500)
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := captureSnapshotState(ctx, socket, root, "memory", "state", os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("partial state accepted after failed API")
	}
}

func TestBoundedSnapshotStateCancellationWithWriter(t *testing.T) {
	root := t.TempDir()
	entered := make(chan struct{})
	finished := make(chan struct{})
	socket := serveSnapshotAPI(t, root, func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
			close(entered)
			return
		}
		state, err := os.OpenFile(filepath.Join(root, "state.pipe"), os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			close(entered)
			return
		}
		defer state.Close()
		_, err = state.Write([]byte("partial state"))
		if err != nil {
			t.Error(err)
		}
		close(entered)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- captureSnapshotState(ctx, socket, root, "memory", "state", os.Getuid(), os.Getgid()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("capture did not join drain with active writer")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("API handler did not finish")
	}
}
