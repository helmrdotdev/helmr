//go:build linux

package worker

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPrepareVerifierHostRequiresDelegatedSupervisor(t *testing.T) {
	root := verifierHostFixture(t, "/system.slice/helmr-worker.service/supervisor", 42)
	if _, err := checkVerifierHost(
		filepath.Join(root, "proc-cgroup"),
		filepath.Join(root, "cgroup"),
		42,
		false,
	); err == nil || !strings.Contains(err.Error(), `"cpu" was not enabled`) {
		t.Fatalf("error before prepare = %v", err)
	}
	cgroupRoot, err := prepareVerifierHost(
		filepath.Join(root, "proc-cgroup"),
		filepath.Join(root, "cgroup"),
		42,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := filepath.Join(root, "cgroup/system.slice/helmr-worker.service")
	if cgroupRoot != wantRoot {
		t.Fatalf("cgroup root = %q, want %q", cgroupRoot, wantRoot)
	}
	raw, err := os.ReadFile(filepath.Join(
		root,
		"cgroup/system.slice/helmr-worker.service/cgroup.subtree_control",
	))
	if err != nil {
		t.Fatal(err)
	}
	for _, controller := range verifierControllers {
		if !strings.Contains(string(raw), "+"+controller) {
			t.Fatalf("subtree control = %q, missing %s", raw, controller)
		}
	}
}

func TestPrepareVerifierHostRejectsProcessInUnitRoot(t *testing.T) {
	root := verifierHostFixture(t, "/system.slice/helmr-worker.service/supervisor", 42)
	unit := filepath.Join(root, "cgroup/system.slice/helmr-worker.service")
	if err := os.WriteFile(filepath.Join(unit, "cgroup.procs"), []byte("7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := prepareVerifierHost(filepath.Join(root, "proc-cgroup"), filepath.Join(root, "cgroup"), 42)
	if err == nil || !strings.Contains(err.Error(), "not process-free") {
		t.Fatalf("error = %v", err)
	}
}

func TestPrepareVerifierHostRejectsMissingController(t *testing.T) {
	root := verifierHostFixture(t, "/system.slice/helmr-worker.service/supervisor", 42)
	unit := filepath.Join(root, "cgroup/system.slice/helmr-worker.service")
	if err := os.WriteFile(filepath.Join(unit, "cgroup.controllers"), []byte("cpu memory"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := prepareVerifierHost(filepath.Join(root, "proc-cgroup"), filepath.Join(root, "cgroup"), 42)
	if err == nil || !strings.Contains(err.Error(), `"pids" is not delegated`) {
		t.Fatalf("error = %v", err)
	}
}

func TestUnifiedCgroupPathRejectsHybridHierarchy(t *testing.T) {
	_, err := unifiedCgroupPath([]byte("0::/system.slice/helmr-worker.service/supervisor\n2:cpu:/legacy\n"))
	if err == nil {
		t.Fatal("hybrid hierarchy was accepted")
	}
}

func verifierHostFixture(t *testing.T, current string, pid int) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "proc-cgroup"), []byte("0::"+current+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(root, "cgroup", strings.TrimPrefix(filepath.Dir(current), "/"))
	supervisor := filepath.Join(root, "cgroup", strings.TrimPrefix(current, "/"))
	if err := os.MkdirAll(supervisor, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(unit, "cgroup.procs"):           "",
		filepath.Join(unit, "cgroup.controllers"):     "cpu cpuset io memory pids",
		filepath.Join(unit, "cgroup.subtree_control"): "",
		filepath.Join(unit, "cgroup.kill"):            "",
		filepath.Join(unit, "memory.swap.max"):        "max",
		filepath.Join(unit, "memory.peak"):            "0",
		filepath.Join(supervisor, "cgroup.procs"):     strconv.Itoa(pid) + "\n",
	}
	for path, contents := range files {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
