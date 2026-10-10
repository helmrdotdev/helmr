//go:build linux

package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/vm"
)

func TestCleanupRequiresCanonicalExactOwnership(t *testing.T) {
	stateDir := t.TempDir()
	jailerDir := t.TempDir()
	id := "019fc619-8443-77f6-9498-8c348c25f701"
	statePath := filepath.Join(stateDir, id)
	if err := os.MkdirAll(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "owner"), []byte(string(vm.OwnerInstance)+"\n"+strings.ToUpper(id)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	connector := &Connector{cfg: Config{StateDir: stateDir, JailerChrootBaseDir: jailerDir, IPPath: "/bin/true"}}
	err := connector.cleanup(context.Background(), vm.Owner{Kind: vm.OwnerInstance, ID: id})
	var unproven *vm.CleanupUnprovenError
	if !errors.As(err, &unproven) || unproven.Owner != (vm.Owner{Kind: vm.OwnerInstance, ID: id}) || !strings.Contains(err.Error(), "ownership marker") {
		t.Fatalf("Cleanup() error = %v, want typed exact ownership rejection", err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("mismatched owner state was removed: %v", err)
	}
	if err := connector.cleanup(context.Background(), vm.Owner{Kind: vm.OwnerInstance, ID: strings.ToUpper(id)}); !errors.As(err, &unproven) {
		t.Fatal("non-canonical owner id was accepted")
	}
}

func TestCleanupRemovesExactInstanceOwnerAndMarkerLast(t *testing.T) {
	stateDir := t.TempDir()
	jailerDir := t.TempDir()
	id := "019fc619-8443-77f6-9498-8c348c25f702"
	statePath := filepath.Join(stateDir, id)
	jailerPath := filepath.Join(jailerDir, "firecracker", id)
	if err := os.MkdirAll(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(jailerPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "owner"), []byte(string(vm.OwnerInstance)+"\n"+id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "scratch"), []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	connector := &Connector{cfg: Config{StateDir: stateDir, JailerChrootBaseDir: jailerDir, IPPath: "/bin/true"}}
	if err := connector.cleanup(context.Background(), vm.Owner{Kind: vm.OwnerInstance, ID: id}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{statePath, jailerPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("cleanup path remains %s: %v", path, err)
		}
	}
}

func TestCleanupInventoryAndStopFailureRetainCustody(t *testing.T) {
	for _, failure := range []string{"inventory", "stop", "rescan", "remaining", "success"} {
		t.Run(failure, func(t *testing.T) {
			c, owner, jail, device := cleanupProcessFixture(t)
			retained := c.lockComputerOwner(owner)
			defer retained.mu.Unlock()
			calls, stops := 0, 0
			failed := errors.New("process proof unavailable")
			inventory := func() ([]int, error) {
				calls++
				if _, err := os.Stat(jail); err != nil {
					t.Fatal("jail removed before process absence", err)
				}
				if device.closes != 0 {
					t.Fatal("device released before process absence")
				}
				if calls == 1 {
					if failure == "inventory" {
						return nil, failed
					}
					return []int{42}, nil
				}
				if stops != 1 {
					t.Fatalf("rescan preceded stop: %d", stops)
				}
				if failure == "rescan" {
					return nil, failed
				}
				if failure == "remaining" {
					return []int{42}, nil
				}
				return nil, nil
			}
			stop := func(context.Context, int) error {
				stops++
				if failure == "stop" {
					return failed
				}
				return nil
			}
			err := c.cleanupOwnedProcesses(t.Context(), owner, retained, inventory, stop)
			if failure == "success" {
				if err != nil || calls != 2 || stops != 1 || device.closes != 1 {
					t.Fatalf("cleanup: %v calls=%d stops=%d closes=%d", err, calls, stops, device.closes)
				}
				return
			}
			var unproven *vm.CleanupUnprovenError
			if !errors.As(err, &unproven) {
				t.Fatalf("unproved cleanup accepted: %v", err)
			}
			if failure != "remaining" && !errors.Is(err, failed) {
				t.Fatalf("lost error identity: %v", err)
			}
			if _, err := os.Stat(jail); err != nil {
				t.Fatal("lost jail", err)
			}
			if err := validateOwnerMarker(filepath.Join(c.cfg.StateDir, owner.ID), owner); err != nil {
				t.Fatal(err)
			}
			if device.closes != 0 {
				t.Fatal("released without process proof")
			}
			select {
			case <-device.excluded:
				t.Fatal("false consumer exclusion")
			default:
			}
		})
	}
}
