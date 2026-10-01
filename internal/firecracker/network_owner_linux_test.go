//go:build linux

package firecracker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
)

func TestNetworkOwnerManifestIsExactAndAtomicallyReplaceable(t *testing.T) {
	connector := &Connector{cfg: Config{
		NetworkLinkPool: "198.18.0.0/29", NetworkTranslationPool: "198.19.0.0/30",
		NetworkResolverIPv4: "1.1.1.1", NetworkCapacity: 2,
	}}
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: "019c10d5-a6f7-7af1-8f5f-000000000021"}
	manifest, err := connector.networkOwnerManifest(owner, 7, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := connector.validateNetworkOwnerManifest(manifest, owner); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), networkManifestName)
	if err := writeNetworkOwnerManifest(path, manifest, true); err != nil {
		t.Fatal(err)
	}
	if err := writeNetworkOwnerManifest(path, manifest, true); err == nil {
		t.Fatal("exclusive crash anchor was overwritten")
	}
	manifest.Installed = true
	manifest.RootIfindex = 11
	manifest.NamespaceIfindex = 12
	manifest.TapIfindex = 13
	manifest.NamespacePolicyHash = "namespace-policy"
	manifest.RootPolicyHash = "root-policy"
	manifest.BPFProgramID = 14
	manifest.BPFProgramTag = "program-tag"
	manifest.BPFFilterHandle = 15
	manifest.PacketMark = 16
	if err := connector.validateNetworkOwnerManifest(manifest, owner); err != nil {
		t.Fatal(err)
	}
	if err := writeNetworkOwnerManifest(path, manifest, false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted networkOwnerManifest
	if err := json.Unmarshal(raw, &persisted); err != nil || persisted != manifest {
		t.Fatalf("persisted manifest = %+v, error = %v", persisted, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != networkManifestName {
		t.Fatalf("manifest directory contains replacement residue: %v", entries)
	}
	changed := manifest
	changed.GuestMAC = "02:fc:00:00:00:03"
	if err := connector.validateNetworkOwnerManifest(changed, owner); err == nil {
		t.Fatal("changed guest identity was accepted")
	}
	changed = manifest
	changed.RootIfindex = 0
	if err := connector.validateNetworkOwnerManifest(changed, owner); err == nil {
		t.Fatal("incomplete installed identity was accepted")
	}
}

func TestNetworkAllocationLockIsStableOutsideOwnerStateRoot(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "guest")
	connector := &Connector{cfg: Config{
		StateDir: stateDir, NetworkLinkPool: "198.18.0.0/29",
		NetworkTranslationPool: "198.19.0.0/30", NetworkResolverIPv4: "1.1.1.1",
		NetworkCapacity: 2,
	}}
	owners := []vm.Owner{
		{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()},
		{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()},
	}
	for _, owner := range owners {
		if _, err := createOwnerStateRoot(stateDir, owner); err != nil {
			t.Fatal(err)
		}
	}
	binding := func(owner vm.Owner) vm.WorkloadBinding {
		return vm.WorkloadBinding{
			WorkerEpoch: 1, OwnerID: owner.ID, Generation: 1,
			ComputerInstanceID: owner.ID, VMPlatformID: vmplatform.Contract,
		}
	}
	first, err := connector.allocateNetworkOwner(owners[0], binding(owners[0]))
	if err != nil {
		t.Fatal(err)
	}
	lockPath := networkAllocationLockPath(stateDir)
	before, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := connector.allocateNetworkOwner(owners[1], binding(owners[1]))
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("network allocation replaced the persistent lock inode")
	}
	if first.AllocationIndex == second.AllocationIndex {
		t.Fatalf("allocations reused index %d", first.AllocationIndex)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(owners) {
		t.Fatalf("owner state entries = %v", entries)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Fatalf("non-owner file entered state root: %q", entry.Name())
		}
	}
}

func TestNetworkAllocationRejectsSymlinkLock(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "guest")
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
	if _, err := createOwnerStateRoot(stateDir, owner); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, networkAllocationLockPath(stateDir)); err != nil {
		t.Fatal(err)
	}
	connector := &Connector{cfg: Config{
		StateDir: stateDir, NetworkLinkPool: "198.18.0.0/29",
		NetworkTranslationPool: "198.19.0.0/30", NetworkResolverIPv4: "1.1.1.1",
		NetworkCapacity: 1,
	}}
	_, err := connector.allocateNetworkOwner(owner, vm.WorkloadBinding{
		WorkerEpoch: 1, OwnerID: owner.ID, Generation: 1,
		ComputerInstanceID: owner.ID, VMPlatformID: vmplatform.Contract,
	})
	if err == nil || !strings.Contains(err.Error(), "open network allocation lock") {
		t.Fatalf("error = %v", err)
	}
}
