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
