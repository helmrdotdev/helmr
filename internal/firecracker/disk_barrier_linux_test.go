//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	sdk "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/helmrdotdev/helmr/internal/vm"
)

func TestPausedDiskBarrierVerifiesActualDevices(t *testing.T) {
	for _, scenario := range []string{"valid", "restored scratch", "restored scratch wrong path", "restored scratch wrong inode", "missing cache", "unsafe computer", "unsafe scratch", "async read", "async write", "missing engine", "extra writer", "missing disk", "duplicate", "wrong path", "wrong inode", "missing source", "oversize", "http error"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			jail := filepath.Join(root, "jail")
			if err := os.Mkdir(jail, 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"computer.ext4", "scratch.ext4"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte("retained data"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(filepath.Join(root, name), filepath.Join(jail, name)); err != nil {
					t.Fatal(err)
				}
			}
			scratch := filepath.Join(root, scratchDiskName)
			if strings.HasPrefix(scenario, "restored scratch") {
				// Restore unpacks to a unique host name but retains the canonical
				// jailed name from the snapshot. Both paths own the same inode.
				restored := filepath.Join(root, "restore-123."+scratchDiskName)
				if err := os.Rename(scratch, restored); err != nil {
					t.Fatal(err)
				}
				scratch = restored
			}
			files, err := openRuntimeDiskFiles(scratch, filepath.Join(root, "computer.ext4"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = closeRuntimeDiskFiles(files) })
			drives := []map[string]any{
				{"drive_id": "computer", "io_engine": "Sync", "is_read_only": false, "cache_type": "Writeback", "path_on_host": "/computer.ext4"},
				{"drive_id": "scratch", "io_engine": "Sync", "is_read_only": false, "cache_type": "Writeback", "path_on_host": "scratch.ext4"},
				{"drive_id": "rootfs", "io_engine": "Sync", "is_read_only": true, "path_on_host": "root.squashfs"},
			}
			switch scenario {
			case "restored scratch wrong path":
				// Even another hard link to the owned inode is not the declared
				// snapshot drive path.
				if err := os.Link(scratch, filepath.Join(jail, filepath.Base(scratch))); err != nil {
					t.Fatal(err)
				}
				drives[1]["path_on_host"] = filepath.Base(scratch)
			case "restored scratch wrong inode":
				if err := os.Remove(filepath.Join(jail, scratchDiskName)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(jail, scratchDiskName), []byte("replacement"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing cache":
				delete(drives[0], "cache_type")
			case "unsafe computer":
				drives[0]["cache_type"] = "Unsafe"
			case "unsafe scratch":
				drives[1]["cache_type"] = "Unsafe"
			case "async read":
				drives[2]["io_engine"] = "Async"
			case "async write":
				drives[0]["io_engine"] = "Async"
			case "missing engine":
				delete(drives[0], "io_engine")
			case "extra writer":
				drives[2]["is_read_only"] = false
			case "missing disk":
				drives = drives[1:]
			case "duplicate":
				drives = append(drives, drives[0])
			case "wrong path":
				drives[0]["path_on_host"] = "../computer.ext4"
			case "wrong inode":
				if err := os.Remove(filepath.Join(jail, "computer.ext4")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(jail, "computer.ext4"), []byte("replacement"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing source":
				if err := os.Remove(filepath.Join(root, "computer.ext4")); err != nil {
					t.Fatal(err)
				}
			}
			socket := serveSnapshotAPI(t, root, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/vm/config" {
					t.Error("unexpected API mutation")
					w.WriteHeader(400)
					return
				}
				if scenario == "http error" {
					w.WriteHeader(500)
					return
				}
				if scenario == "oversize" {
					_, _ = w.Write(make([]byte, 65537))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"drives": drives})
			})
			machine, err := sdk.NewMachine(context.Background(), sdk.Config{SocketPath: socket})
			if err != nil {
				t.Fatal(err)
			}
			session := &guestSession{diskFiles: files, machine: machine, jailRoot: jail, scratchDisk: scratch, topology: vm.RuntimeTopology{Computer: &vm.RuntimeComputer{Path: filepath.Join(root, "computer.ext4")}}}
			err = session.syncPausedDisks(t.Context())
			if (err == nil) != (scenario == "valid" || scenario == "restored scratch") {
				t.Fatalf("barrier result: %v", err)
			}
		})
	}
}

func TestPausedBackingSyncErrorIsReturned(t *testing.T) {
	// This regular procfs control is openable but cannot be fsynced. No writes
	// are made; exercise the real syscall error instead of a fake sync success.
	path := "/proc/self/coredump_filter"
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := syncPausedBacking(file, path, path); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("backing sync failure was not preserved: %v", err)
	}
}

func TestRuntimeDrivesPinSynchronousIO(t *testing.T) {
	for _, computer := range []string{"", "computer.ext4"} {
		for _, drive := range runtimeDrivesWithComputer("root", "scratch", "substrate", computer, nil, nil) {
			if !*drive.IsReadOnly && (drive.CacheType == nil || *drive.CacheType != "Writeback") {
				t.Fatalf("guest flush disabled: %+v", drive)
			}
			if drive.IoEngine == nil || *drive.IoEngine != "Sync" {
				t.Fatalf("unqualified device: %+v", drive)
			}
		}
	}
}

func TestRuntimeDiskDescriptorsHaveOneOwner(t *testing.T) {
	root := t.TempDir()
	scratch := filepath.Join(root, "scratch.ext4")
	if err := os.WriteFile(scratch, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	files, err := openRuntimeDiskFiles(scratch, filepath.Join(root, "missing"))
	if err == nil {
		t.Fatal("accepted missing Computer")
	}
	if _, err := files[scratchDriveID].Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("failed startup leaked descriptor: %v", err)
	}
	files, err = openRuntimeDiskFiles(scratch, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := closeRuntimeDiskFiles(files); err != nil {
		t.Fatal(err)
	}
	if err := syncPausedBacking(files[scratchDriveID], scratch, scratch); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed owner used for capture: %v", err)
	}
}
