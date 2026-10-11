package custody

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

func makeRecoveryOwner(t *testing.T, work, jailer, id string) string {
	t.Helper()
	state := filepath.Join(work, "vms", "guest", id)
	root := filepath.Join(jailer, "firecracker", id, "root")
	for _, path := range []string{state, root} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(state, "owner"), []byte("instance\n"+id+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRecoveryProcessUsesOwnedDirectoryIdentity(t *testing.T) {
	const id = "019c10d5-a6f7-7af1-8f5f-000000000107"
	for _, tc := range []struct {
		name                          string
		command                       string
		otherRoot, problem, unrelated bool
	}{
		{name: "root alias", command: "/firecracker\x00"},
		{name: "matching command", command: "/firecracker\x00--id\x00" + id + "\x00"},
		{name: "contradicting command", command: "/firecracker\x00--id\x00other\x00", problem: true},
		{name: "command alone", command: "/firecracker\x00--id\x00" + id + "\x00", otherRoot: true, problem: true},
		{name: "foreign root", command: "/firecracker\x00", otherRoot: true, unrelated: true},
		{name: "unrelated program", command: "/unrelated\x00", unrelated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work, jailer, proc := t.TempDir(), t.TempDir(), t.TempDir()
			root := makeRecoveryOwner(t, work, jailer, id)
			if tc.otherRoot {
				root = t.TempDir()
			}
			alias := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(root, alias); err != nil {
				t.Fatal(err)
			}
			pid := filepath.Join(proc, "42")
			if err := os.Mkdir(pid, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pid, "cmdline"), []byte(tc.command), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(alias, filepath.Join(pid, "root")); err != nil {
				t.Fatal(err)
			}
			got, err := processesAt(proc, StateRoot{Base: work, Components: []string{"vms", "guest"}}, jailer)
			if err != nil {
				t.Fatal(err)
			}
			if tc.unrelated {
				if len(got) != 0 {
					t.Fatalf("selected unrelated process: %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].ID != id || (got[0].Problem != "") != tc.problem {
				t.Fatalf("inventory=%+v", got)
			}
		})
	}
}

func TestRecoveryRejectsUntrustedOwnerPaths(t *testing.T) {
	const id = "019c10d5-a6f7-7af1-8f5f-000000000107"
	for _, path := range []string{"vms", "vms/guest", "vms/guest/" + id, "vms/guest/" + id + "/owner", "jail", "marker contents", "missing marker"} {
		t.Run(path, func(t *testing.T) {
			work, jailer := t.TempDir(), t.TempDir()
			root := makeRecoveryOwner(t, work, jailer, id)
			marker := filepath.Join(work, "vms", "guest", id, "owner")
			switch path {
			case "marker contents":
				if err := os.WriteFile(marker, []byte("instance\n019c10d5-a6f7-7af1-8f5f-000000000108\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing marker":
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
			default:
				target := filepath.Join(work, path)
				if path == "jail" {
					target = root
				}
				moved := filepath.Join(t.TempDir(), "moved")
				if err := os.Rename(target, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, target); err != nil {
					t.Fatal(err)
				}
			}
			roots, err := openRecoveryRoots(StateRoot{Base: work, Components: []string{"vms", "guest"}}, jailer)
			defer closeRecoveryRoots(roots)
			if err == nil && len(roots) != 0 {
				t.Fatalf("untrusted path granted authority: %+v", roots)
			}
		})
	}
}

func TestProcessDisappearanceDistinguishesReadFailures(t *testing.T) {
	live := t.TempDir()
	missing := filepath.Join(live, "gone")
	for _, tc := range []struct {
		name, path string
		err        error
		gone       bool
	}{
		{"dead procfs task", live, &os.PathError{Op: "read", Path: filepath.Join(live, "cmdline"), Err: syscall.ESRCH}, true},
		{"disappeared process", missing, os.ErrNotExist, true},
		{"wrapped disappeared process", missing, fmt.Errorf("read process: %w", os.ErrNotExist), true},
		{"wrapped live missing file", live, fmt.Errorf("read process: %w", os.ErrNotExist), false},
		{"missing live command line", live, os.ErrNotExist, false},
		{"denied live process", live, os.ErrPermission, false},
		{"failed process read", missing, syscall.EIO, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := processDisappeared(tc.path, tc.err); got != tc.gone {
				t.Fatalf("disappeared=%v", got)
			}
		})
	}
}

func TestProcessInventoryReadsRootOnlyForFirecracker(t *testing.T) {
	procDir, work, jailer := t.TempDir(), t.TempDir(), t.TempDir()
	id := "019c10d5-a6f7-7af1-8f5f-000000000107"
	makeRecoveryOwner(t, work, jailer, id)
	for pid, command := range map[string]string{
		"42": "/usr/bin/jailer\x00--id\x00" + id + "\x00--chroot-base-dir\x00" + jailer + "\x00",
		"43": "/usr/bin/firecracker\x00",
		"44": "/usr/bin/unrelated\x00",
	} {
		path := filepath.Join(procDir, pid)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "cmdline"), []byte(command), 0600); err != nil {
			t.Fatal(err)
		}
		if pid == "43" {
			if err := os.Symlink(filepath.Join(jailer, "firecracker", id, "root"), filepath.Join(path, "root")); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := processesAt(procDir, StateRoot{Base: work, Components: []string{"vms", "guest"}}, jailer)
	if err != nil {
		t.Fatal(err)
	}
	want := []Process{{PID: 42, ID: id}, {PID: 43, ID: id}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("owned processes=%+v", got)
	}
}

func TestProcessInventoryFailures(t *testing.T) {
	for _, failure := range []string{"missing procfs", "unreadable cmdline", "missing live cmdline", "unreadable firecracker root"} {
		t.Run(failure, func(t *testing.T) {
			procDir := t.TempDir()
			pidDir := filepath.Join(procDir, "42")
			if failure == "missing procfs" {
				procDir = filepath.Join(procDir, "missing")
			} else {
				if err := os.Mkdir(pidDir, 0700); err != nil {
					t.Fatal(err)
				}
				switch failure {
				case "unreadable cmdline":
					if err := os.Mkdir(filepath.Join(pidDir, "cmdline"), 0700); err != nil {
						t.Fatal(err)
					}
				case "unreadable firecracker root":
					if err := os.WriteFile(filepath.Join(pidDir, "cmdline"), []byte("/usr/bin/firecracker\x00"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(pidDir, "root"), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err := processesAt(procDir, StateRoot{Base: t.TempDir()}, t.TempDir())
			if err == nil {
				t.Fatal("incomplete process inventory accepted")
			}
		})
	}
}
