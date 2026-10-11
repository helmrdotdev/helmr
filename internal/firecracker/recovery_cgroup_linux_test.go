//go:build linux

package firecracker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/worker"
)

func TestRecoveryCgroupRequiresExactMarker(t *testing.T) {
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: "019fc619-8443-77f6-9498-8c348c25f710"}
	for _, kind := range []string{"missing", "mismatch", "root-symlink", "marker-symlink", "bad-hierarchy"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			state := filepath.Join(root, owner.ID)
			if err := os.Mkdir(state, 0700); err != nil {
				t.Fatal(err)
			}
			content := []byte(string(owner.Kind) + "\n" + owner.ID + "\n")
			marker := filepath.Join(state, "owner")
			switch kind {
			case "mismatch":
				content = []byte("instance\n019fc619-8443-77f6-9498-8c348c25f711\n")
			case "root-symlink":
				if err := os.Remove(state); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), state); err != nil {
					t.Fatal(err)
				}
			case "marker-symlink":
				target := filepath.Join(t.TempDir(), "marker")
				if err := os.WriteFile(target, content, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, marker); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "missing" && kind != "marker-symlink" {
				if err := os.WriteFile(marker, content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			version := "2"
			if kind == "bad-hierarchy" {
				version = "invalid"
			}
			err := RemoveStoppedCgroup(root, version, owner)
			if err == nil {
				t.Fatal("accepted unproven cgroup removal")
			}
			if kind != "bad-hierarchy" && !strings.Contains(err.Error(), "owner") {
				t.Fatalf("reached cgroup lookup without exact ownership: %v", err)
			}
			if _, err := os.Lstat(state); err != nil {
				t.Fatalf("changed evidence: %v", err)
			}
		})
	}
}

func TestRecoveryExactCgroup(t *testing.T) {
	if os.Getenv("HELMR_PRIVILEGED_CGROUP_TEST") != "1" {
		t.Skip("requires disposable writable cgroup namespace")
	}
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: "019fc619-8443-77f6-9498-8c348c25f712"}
	path, err := runtimeCgroupPath("2", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(path)
	if err := os.Mkdir(parent, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(parent); err != nil {
			t.Error(err)
		}
	})
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	neighbor := filepath.Join(parent, "unrelated")
	if err := os.Mkdir(neighbor, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(neighbor); err != nil {
			t.Error(err)
		}
	})
	work, jailer := t.TempDir(), t.TempDir()
	root := filepath.Join(work, "vms", "guest")
	state := filepath.Join(root, owner.ID)
	jail := filepath.Join(jailer, "firecracker", owner.ID)
	for _, dir := range []string{state, jail} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(state, "owner")
	if err := os.WriteFile(marker, []byte(string(owner.Kind)+"\n"+owner.ID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	recover := func() worker.RecoveryEvidence {
		evidence, err := worker.RecoverLocalVMState(t.Context(), work, jailer, "/bin/true",
			func(context.Context, vm.Owner) error { return nil },
			func(owner vm.Owner) error { return RemoveStoppedCgroup(root, "2", owner) })
		if err != nil {
			t.Fatal(err)
		}
		return evidence
	}
	assertBlocked := func() {
		t.Helper()
		evidence := recover()
		if len(evidence.Reclaimed) != 0 || len(evidence.QuarantinedOwners) != 1 || evidence.QuarantinedOwners[0] != owner {
			t.Fatalf("cgroup residue reclaimed: %+v", evidence)
		}
		for _, kept := range []string{marker, jail, path, neighbor} {
			if _, err := os.Lstat(kept); err != nil {
				t.Fatalf("lost evidence %s: %v", kept, err)
			}
		}
	}
	markerBytes, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	missingOwner := recover()
	if len(missingOwner.Reclaimed) != 0 || len(missingOwner.Quarantined) == 0 {
		t.Fatalf("missing owner permitted recovery: %+v", missingOwner)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("ownerless cgroup changed", err)
	}
	if err := os.WriteFile(marker, markerBytes, 0600); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(path, "child")
	if err := os.Mkdir(child, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(child) })
	assertBlocked()
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	consumer := exec.Command("sleep", "60")
	if err := consumer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if consumer.ProcessState == nil {
			_ = consumer.Process.Kill()
			_ = consumer.Wait()
		}
	})
	if err := os.WriteFile(filepath.Join(path, "cgroup.procs"), []byte(strconv.Itoa(consumer.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	assertBlocked()
	if err := consumer.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = consumer.Wait()
	evidence := recover()
	if len(evidence.Quarantined) != 0 || len(evidence.Reclaimed) != 1 || evidence.Reclaimed[0] != owner.ID {
		t.Fatalf("exact cleanup did not unblock recovery: %+v", evidence)
	}
	for _, gone := range []string{state, jail, path} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Fatalf("not absent %s: %v", gone, err)
		}
	}
	if _, err := os.Lstat(neighbor); err != nil {
		t.Fatal("changed unrelated cgroup", err)
	}
}
