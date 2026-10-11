//go:build linux

package firecracker

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/vm"
)

func TestInstanceIdentitySurvivesCleanupAndProcessRestart(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "vms")
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
	state, err := createOwnerStateRoot(stateDir, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := removeStateRootLast(state, owner); err != nil {
		t.Fatal(err)
	}
	// A separate process has no retained Go locks or ownership map.
	cmd := exec.Command(os.Args[0], "-test.run=^TestInstanceIdentityClaimChild$")
	cmd.Env = append(os.Environ(), "HELMR_TEST_INSTANCE_STATE="+stateDir, "HELMR_TEST_INSTANCE_ID="+owner.ID)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh process accepted spent identity: %v\n%s", err, output)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry recreated runtime state: %v", err)
	}
	owner.ID = uuid.NewV7().String()
	if _, err := createOwnerStateRoot(stateDir, owner); err != nil {
		t.Fatalf("replacement identity rejected: %v", err)
	}
}

func TestInstanceIdentityClaimChild(t *testing.T) {
	stateDir := os.Getenv("HELMR_TEST_INSTANCE_STATE")
	if stateDir == "" {
		t.Skip("subprocess fixture")
	}
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: os.Getenv("HELMR_TEST_INSTANCE_ID")}
	if _, err := createOwnerStateRoot(stateDir, owner); !errors.Is(err, os.ErrExist) {
		t.Fatalf("claim retry = %v", err)
	}
}

func TestInstanceIdentityConcurrentClaims(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "vms")
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
	start := make(chan struct{})
	results := make(chan error, 16)
	var group sync.WaitGroup
	for range cap(results) {
		group.Go(func() { <-start; _, err := createOwnerStateRoot(stateDir, owner); results <- err })
	}
	close(start)
	group.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, os.ErrExist) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("physical launch claims = %d", winners)
	}
}

func TestInstanceIdentityRetainedAfterStateCreationFailure(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "vms")
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(stateDir, owner.ID)
	if err := os.WriteFile(state, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := createOwnerStateRoot(stateDir, owner); !errors.Is(err, os.ErrExist) {
		t.Fatalf("state collision = %v", err)
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	if _, err := createOwnerStateRoot(stateDir, owner); !errors.Is(err, os.ErrExist) {
		t.Fatalf("failed launch identity reused: %v", err)
	}
}
