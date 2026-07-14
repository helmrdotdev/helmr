package worker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRuntimeCandidatesUnionAllOwnedRootsIncludingJailerOnlyOrphan(t *testing.T) {
	workDir := t.TempDir()
	jailerDir := t.TempDir()
	workOnly := "00000000-0000-0000-0000-000000000101"
	shared := "00000000-0000-0000-0000-000000000102"
	jailerOnly := "00000000-0000-0000-0000-000000000103"
	for _, path := range []string{
		filepath.Join(workDir, "vms", "guest", workOnly),
		filepath.Join(workDir, "vms", "guest", shared),
		filepath.Join(jailerDir, "firecracker", shared),
		filepath.Join(jailerDir, "firecracker", jailerOnly),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	got, err := runtimeCandidates(workDir, jailerDir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{workOnly, shared, jailerOnly}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtimeCandidates() = %v, want %v", got, want)
	}
}

func TestRuntimeCandidatesRejectsNonCanonicalOwnershipInsteadOfCertifyingItAway(t *testing.T) {
	for _, test := range []struct {
		name     string
		rootKind string
		entry    string
	}{
		{name: "embedded uuid", rootKind: "work", entry: "orphan-00000000-0000-0000-0000-000000000101"},
		{name: "uppercase uuid", rootKind: "work", entry: "00000000-0000-0000-0000-000000000ABC"},
		{name: "jailer unknown owner", rootKind: "jailer", entry: "not-a-runtime"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workDir := t.TempDir()
			jailerDir := t.TempDir()
			root := filepath.Join(workDir, "vms", "guest")
			if test.rootKind == "jailer" {
				root = filepath.Join(jailerDir, "firecracker")
			}
			if err := os.MkdirAll(filepath.Join(root, test.entry), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := runtimeCandidates(workDir, jailerDir); err == nil || !strings.Contains(err.Error(), "non-canonical runtime id") {
				t.Fatalf("runtimeCandidates() error = %v, want non-canonical ownership failure", err)
			}
		})
	}
}

func TestRecoveryReclaimsJailerOnlyCrashOrphan(t *testing.T) {
	workDir := t.TempDir()
	jailerDir := t.TempDir()
	id := "00000000-0000-0000-0000-000000000104"
	jailerPath := filepath.Join(jailerDir, "firecracker", id)
	if err := os.MkdirAll(jailerPath, 0o700); err != nil {
		t.Fatal(err)
	}

	evidence, err := recoverLocalRuntimeState(context.Background(), workDir, jailerDir, runtimeRecoveryOps{
		candidates:   func(context.Context) ([]string, error) { return runtimeCandidates(workDir, jailerDir) },
		matchingPIDs: func(string) ([]int, error) { return nil, nil },
		stopPID:      func(context.Context, int) error { return nil },
		netnsExists:  func(context.Context, string) (bool, error) { return false, nil },
		deleteNetns:  func(context.Context, string) error { return nil },
		removeAll:    os.RemoveAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evidence.Reclaimed, []string{id}) || len(evidence.Quarantined) != 0 {
		t.Fatalf("evidence = %+v", evidence)
	}
	if _, err := os.Stat(jailerPath); !os.IsNotExist(err) {
		t.Fatalf("jailer-only orphan remains after recovery: %v", err)
	}
}

func TestRecoveryFindsOwnedProcessAndCorrelatedNetNSWithoutStateRoots(t *testing.T) {
	workDir := t.TempDir()
	jailerDir := t.TempDir()
	id := "00000000-0000-0000-0000-000000000106"
	unrelated := "00000000-0000-0000-0000-000000000999"
	var stopped []int
	var deleted []string
	var matched []string
	evidence, err := recoverLocalRuntimeState(context.Background(), workDir, jailerDir, runtimeRecoveryOps{
		candidates: func(context.Context) ([]string, error) { return nil, nil },
		ownedProcesses: func(context.Context) ([]ownedRuntimeProcess, error) {
			return []ownedRuntimeProcess{{PID: 42, ID: id}}, nil
		},
		netnsNames: func(context.Context) ([]string, error) { return []string{id, unrelated}, nil },
		matchingPIDs: func(candidate string) ([]int, error) {
			matched = append(matched, candidate)
			if candidate == id {
				return []int{42}, nil
			}
			return nil, nil
		},
		stopPID:     func(_ context.Context, pid int) error { stopped = append(stopped, pid); return nil },
		netnsExists: func(_ context.Context, candidate string) (bool, error) { return candidate == id, nil },
		deleteNetns: func(_ context.Context, candidate string) error { deleted = append(deleted, candidate); return nil },
		removeAll:   os.RemoveAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evidence.Reclaimed, []string{id}) || !reflect.DeepEqual(stopped, []int{42}) || !reflect.DeepEqual(deleted, []string{id}) {
		t.Fatalf("evidence=%+v stopped=%v deleted=%v", evidence, stopped, deleted)
	}
	if !reflect.DeepEqual(matched, []string{id}) {
		t.Fatalf("matched runtime IDs = %v; unrelated netns must not become ownership", matched)
	}
}

func TestRecoveryQuarantinesNonCanonicalPreciselyOwnedProcess(t *testing.T) {
	evidence, err := recoverLocalRuntimeState(context.Background(), t.TempDir(), t.TempDir(), runtimeRecoveryOps{
		candidates: func(context.Context) ([]string, error) { return nil, nil },
		ownedProcesses: func(context.Context) ([]ownedRuntimeProcess, error) {
			return []ownedRuntimeProcess{{PID: 43, ID: "not-a-runtime", Problem: "owned jailer process has non-canonical --id"}}, nil
		},
		netnsNames:   func(context.Context) ([]string, error) { return []string{"not-a-runtime"}, nil },
		matchingPIDs: func(string) ([]int, error) { t.Fatal("unsafe residue was selected for cleanup"); return nil, nil },
		stopPID:      func(context.Context, int) error { return nil },
		netnsExists:  func(context.Context, string) (bool, error) { return false, nil },
		deleteNetns:  func(context.Context, string) error { return nil },
		removeAll:    os.RemoveAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evidence.Quarantined, []string{"process:43"}) || len(evidence.QuarantineErrors) != 1 {
		t.Fatalf("evidence = %+v", evidence)
	}
}

func TestOwnedRuntimeProcessDetectionIgnoresUnrelatedResources(t *testing.T) {
	id := "00000000-0000-0000-0000-000000000107"
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
			id, owned, problem := helmrOwnedRuntimeProcess([]byte(tt.cmdline), tt.root, jailerDir)
			if owned != tt.wantOwned || id != tt.wantID || (problem != "") != tt.wantProblem {
				t.Fatalf("id=%q owned=%t problem=%q", id, owned, problem)
			}
		})
	}
}
