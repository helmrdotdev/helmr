package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/vm"
)

func TestRecoveryQuarantinesProcessWithoutOwnerMarker(t *testing.T) {
	workDir := t.TempDir()
	jailerDir := t.TempDir()
	id := "019c10d5-a6f7-7af1-8f5f-000000000106"
	unrelated := "019c10d5-a6f7-7af1-8f5f-000000000999"
	var stopped []int
	var reclaimed []vm.Owner
	var matched []string
	evidence, err := recoverLocalVMState(context.Background(), workDir, jailerDir, vmRecoveryOps{
		ownerCandidates: func(context.Context) ([]ownerCandidate, error) { return nil, nil },
		ownedProcesses: func(context.Context) ([]ownedVMProcess, error) {
			return []ownedVMProcess{{PID: 42, ID: id}}, nil
		},
		netnsNames: func(context.Context) ([]string, error) { return []string{id, unrelated}, nil },
		matchingPIDs: func(candidate string) ([]int, error) {
			matched = append(matched, candidate)
			if candidate == id {
				return []int{42}, nil
			}
			return nil, nil
		},
		stopPID:        func(_ context.Context, pid int) error { stopped = append(stopped, pid); return nil },
		netnsExists:    func(_ context.Context, candidate string) (bool, error) { return candidate == id, nil },
		reclaimNetwork: func(_ context.Context, owner vm.Owner) error { reclaimed = append(reclaimed, owner); return nil },
		removeAll:      os.RemoveAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evidence.Quarantined, []string{"process:42"}) || len(evidence.QuarantineErrors) != 1 {
		t.Fatalf("evidence=%+v stopped=%v reclaimed=%v", evidence, stopped, reclaimed)
	}
	if len(matched) != 0 || len(stopped) != 0 || len(reclaimed) != 0 {
		t.Fatalf("ownerless process was selected for cleanup: matched=%v stopped=%v reclaimed=%v", matched, stopped, reclaimed)
	}
}

func TestRecoveryQuarantinesMalformedOwnedProcess(t *testing.T) {
	evidence, err := recoverLocalVMState(context.Background(), t.TempDir(), t.TempDir(), vmRecoveryOps{
		ownerCandidates: func(context.Context) ([]ownerCandidate, error) { return nil, nil },
		ownedProcesses: func(context.Context) ([]ownedVMProcess, error) {
			return []ownedVMProcess{{PID: 43, ID: "not-a-runtime", Problem: "owned jailer process has non-canonical --id"}}, nil
		},
		netnsNames:     func(context.Context) ([]string, error) { return []string{"not-a-runtime"}, nil },
		matchingPIDs:   func(string) ([]int, error) { t.Fatal("unsafe residue was selected for cleanup"); return nil, nil },
		stopPID:        func(context.Context, int) error { return nil },
		netnsExists:    func(context.Context, string) (bool, error) { return false, nil },
		reclaimNetwork: func(context.Context, vm.Owner) error { return nil },
		removeAll:      os.RemoveAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evidence.Quarantined, []string{"process:43"}) || len(evidence.QuarantineErrors) != 1 {
		t.Fatalf("evidence = %+v", evidence)
	}
}

func TestOwnedVMProcessDetectionIgnoresUnrelatedResources(t *testing.T) {
	id := "019c10d5-a6f7-7af1-8f5f-000000000107"
	jailerDir := "/srv/helmr/jailer"
	tests := []struct {
		name        string
		cmdline     string
		root        string
		wantOwned   bool
		wantID      string
		wantProblem bool
	}{
		{name: "owned jailer", cmdline: "/usr/bin/jailer\x00--id\x00" + id + "\x00--chroot-base-dir\x00" + jailerDir + "\x00", wantOwned: true, wantID: id},
		{name: "other jailer root", cmdline: "/usr/bin/jailer\x00--id\x00" + id + "\x00--chroot-base-dir\x00/srv/other\x00"},
		{name: "owned firecracker root", cmdline: "/usr/bin/firecracker\x00", root: jailerDir + "/firecracker/" + id + "/root", wantOwned: true, wantID: id},
		{name: "unrelated firecracker", cmdline: "/usr/bin/firecracker\x00--id\x00" + id + "\x00", root: "/"},
		{name: "noncanonical owned jailer", cmdline: "/usr/bin/jailer\x00--id\x00bad\x00--chroot-base-dir\x00" + jailerDir + "\x00", wantOwned: true, wantID: "bad", wantProblem: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, owned, problem := helmrOwnedVMProcess([]byte(tt.cmdline), tt.root, jailerDir)
			if owned != tt.wantOwned || id != tt.wantID || (problem != "") != tt.wantProblem {
				t.Fatalf("id=%q owned=%t problem=%q", id, owned, problem)
			}
		})
	}
}

func TestRecoveryReclaimsRuntimeAndBuildFromExactOwnerMarkers(t *testing.T) {
	workDir := t.TempDir()
	jailerDir := t.TempDir()
	owners := []vm.Owner{
		{Kind: vm.OwnerRuntime, ID: "019c10d5-a6f7-7af1-8f5f-000000000201"},
		{Kind: vm.OwnerBuild, ID: "019c10d5-a6f7-7af1-8f5f-000000000202"},
	}
	for _, owner := range owners {
		statePath := filepath.Join(workDir, "vms", "guest", owner.ID)
		if err := os.MkdirAll(statePath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(statePath, "owner"), []byte(string(owner.Kind)+"\n"+owner.ID+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(jailerDir, "firecracker", owner.ID), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := RecoverLocalVMState(context.Background(), workDir, jailerDir, truePath, func(context.Context, vm.Owner) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evidence.ReclaimedOwners, owners) || len(evidence.Quarantined) != 0 {
		t.Fatalf("evidence = %+v", evidence)
	}
}

func TestRecoveryDoesNotGuessOwnerFromJailerRoot(t *testing.T) {
	workDir := t.TempDir()
	jailerDir := t.TempDir()
	id := "019c10d5-a6f7-7af1-8f5f-000000000203"
	jailerPath := filepath.Join(jailerDir, "firecracker", id)
	if err := os.MkdirAll(jailerPath, 0o700); err != nil {
		t.Fatal(err)
	}
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := RecoverLocalVMState(context.Background(), workDir, jailerDir, truePath, func(context.Context, vm.Owner) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evidence.Quarantined, []string{"jailer:" + id}) || len(evidence.QuarantinedOwners) != 0 {
		t.Fatalf("evidence = %+v", evidence)
	}
	if _, err := os.Stat(jailerPath); err != nil {
		t.Fatalf("ownerless jailer evidence was removed: %v", err)
	}
}

func TestOwnedVMCandidatesIgnoreSiblingCoordinationState(t *testing.T) {
	workDir := t.TempDir()
	stateDir := filepath.Join(workDir, "vms", "guest")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateDir+".network.lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	candidates, err := ownedVMCandidates(workDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("sibling coordination state became VM ownership state: %+v", candidates)
	}
}

func TestOwnedVMCandidatesQuarantineStrayStateEntry(t *testing.T) {
	workDir := t.TempDir()
	stateDir := filepath.Join(workDir, "vms", "guest")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "unexpected-state"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	candidates, err := ownedVMCandidates(workDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || !strings.Contains(candidates[0].Problem, "non-canonical VM id") {
		t.Fatalf("owner-root contamination was not quarantined: %+v", candidates)
	}
}

func TestRecoveryQuarantinePreservesStructuredBuildOwner(t *testing.T) {
	workDir := t.TempDir()
	owner := vm.Owner{Kind: vm.OwnerBuild, ID: "019c10d5-a6f7-7af1-8f5f-000000000204"}
	statePath := filepath.Join(workDir, "vms", "guest", owner.ID)
	if err := os.MkdirAll(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "owner"), []byte("build\n"+owner.ID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence, err := recoverLocalVMState(context.Background(), workDir, "", vmRecoveryOps{
		ownerCandidates: func(context.Context) ([]ownerCandidate, error) {
			return []ownerCandidate{{Owner: owner}}, nil
		},
		matchingPIDs:   func(string) ([]int, error) { return []int{42}, nil },
		stopPID:        func(context.Context, int) error { return errors.New("still running") },
		netnsExists:    func(context.Context, string) (bool, error) { return false, nil },
		reclaimNetwork: func(context.Context, vm.Owner) error { return nil },
		removeAll:      os.RemoveAll,
		removeState:    removeOwnedRecoveryState,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evidence.QuarantinedOwners, []vm.Owner{owner}) || !reflect.DeepEqual(evidence.Quarantined, []string{owner.String()}) {
		t.Fatalf("evidence = %+v", evidence)
	}
	if _, err := os.Stat(filepath.Join(statePath, "owner")); err != nil {
		t.Fatalf("quarantined owner marker was removed: %v", err)
	}
}
