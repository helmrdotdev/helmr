//go:build linux && computerproof

package guestd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Run only in a disposable Linux cgroup namespace with a writable cgroup2
// mount. This proof uses real kernel controls and existing guest primitives;
// it does not prove guest authority isolation, disk durability, or VM restore.
func TestComputerScopesProof(t *testing.T) {
	root, err := os.MkdirTemp("/sys/fs/cgroup", "helmr-computer-proof-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(root); err != nil {
			t.Error(err)
		}
	})
	tree := computerProofScope(t, root, "tree")
	parent := computerProofScope(t, tree.path, "parent-process")
	childTree := computerProofScope(t, tree.path, "child")
	child := computerProofScope(t, childTree.path, "process")
	data := t.TempDir()
	parentPaths := []string{filepath.Join(data, "parent"), filepath.Join(data, "parent.descendant")}
	childPaths := []string{filepath.Join(data, "child"), filepath.Join(data, "child.descendant")}
	all := append(append([]string{}, parentPaths...), childPaths...)
	computerProofWriter(t, parent, parentPaths[0])
	childDone := computerProofWriter(t, child, childPaths[0])
	computerProofGrowth(t, all)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := childTree.freeze(ctx); err != nil {
		t.Fatal(err)
	}
	childFrozen := computerProofContents(t, childPaths)
	computerProofGrowth(t, parentPaths)
	computerProofStable(t, childFrozen)
	if err := childTree.thaw(ctx); err != nil {
		t.Fatal(err)
	}
	computerProofGrowth(t, childPaths)

	// Freeze the whole execution tree while the supervisor remains outside it.
	// Snapshot reads and stability checks model the writer-exclusion boundary,
	// not a durable filesystem capture (kernel I/O flush is separate).
	if err := tree.freeze(ctx); err != nil {
		t.Fatal(err)
	}
	frozen := computerProofContents(t, all)
	computerProofStable(t, frozen)
	// A cancelled child must disappear even while the surviving parent is frozen.
	if err := childTree.kill(); err != nil {
		t.Fatal(err)
	}
	if err := childTree.waitEmpty(); err != nil {
		t.Fatal(err)
	}
	computerProofExited(t, childDone)
	if err := tree.thaw(ctx); err != nil {
		t.Fatal(err)
	}
	computerProofGrowth(t, parentPaths)
	computerProofStable(t, map[string][]byte{childPaths[0]: frozen[childPaths[0]], childPaths[1]: frozen[childPaths[1]]})

	// Admit another child to check root termination with a live nested scope.
	nextTree := computerProofScope(t, tree.path, "next-child")
	next := computerProofScope(t, nextTree.path, "process")
	nextPaths := []string{filepath.Join(data, "next"), filepath.Join(data, "next.descendant")}
	nextDone := computerProofWriter(t, next, nextPaths[0])
	computerProofGrowth(t, nextPaths)
	if err := tree.kill(); err != nil {
		t.Fatal(err)
	}
	if err := tree.waitEmpty(); err != nil {
		t.Fatal(err)
	}
	computerProofExited(t, nextDone)
	computerProofStable(t, computerProofContents(t, append(parentPaths, nextPaths...)))
	t.Log("child freeze preserves parent progress; tree freeze excludes all user writers; frozen child cancellation survives thaw; root kill empties all descendants")
}

func computerProofScope(t *testing.T, parent, name string) *linuxProgramCgroup {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope := &linuxProgramCgroup{path: path, file: os.NewFile(uintptr(fd), path)}
	t.Cleanup(func() {
		if err := scope.kill(); err != nil {
			t.Error(err)
		}
		if err := scope.waitEmpty(); err != nil {
			t.Error(err)
		}
		if err := scope.close(); err != nil {
			t.Error(err)
		}
	})
	return scope
}

func computerProofWriter(t *testing.T, scope *linuxProgramCgroup, path string) <-chan error {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestComputerScopeWriterHelper$")
	cmd.Env = append(os.Environ(), "HELMR_COMPUTER_PROOF_WRITER="+path, "HELMR_COMPUTER_PROOF_DESCENDANT=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := scope.attach(cmd); err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = scope.kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("writer cleanup timed out")
		}
	})
	return done
}

func computerProofExited(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("writer exited without termination")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not terminate")
	}
}

func computerProofContents(t *testing.T, paths []string) map[string][]byte {
	t.Helper()
	values := make(map[string][]byte, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		values[path] = body
	}
	return values
}

func computerProofGrowth(t *testing.T, paths []string) {
	t.Helper()
	before := computerProofContents(t, paths)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current := computerProofContents(t, paths)
		growing := true
		for path, previous := range before {
			if len(current[path]) <= len(previous) {
				growing = false
			}
		}
		if growing {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("writers did not progress: %v", paths)
}

func computerProofStable(t *testing.T, expected map[string][]byte) {
	t.Helper()
	// Multiple observations catch accidental thaw; cgroup.events/populated is
	// the kernel completion signal, rather than elapsed time being a stop proof.
	for range 20 {
		for path, want := range expected {
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("writer modified %s while excluded", path)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestComputerScopeWriterHelper(t *testing.T) {
	path := os.Getenv("HELMR_COMPUTER_PROOF_WRITER")
	if path == "" {
		t.Skip("subprocess helper")
	}
	if os.Getenv("HELMR_COMPUTER_PROOF_DESCENDANT") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestComputerScopeWriterHelper$")
		cmd.Env = append(os.Environ(), "HELMR_COMPUTER_PROOF_WRITER="+path+".descendant", "HELMR_COMPUTER_PROOF_DESCENDANT=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// The kernel cgroup owns this detached descendant, not a process-group kill.
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for {
		if _, err := fmt.Fprintln(file, "progress"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
}
