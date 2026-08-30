//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uuid"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/helmrdotdev/helmr/internal/firecracker/datapath"
	"github.com/helmrdotdev/helmr/internal/runtimeid"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/vishvananda/netlink"
)

func TestRoutedNetworkLifecyclePrivileged(t *testing.T) {
	if os.Getenv("HELMR_NETWORK_E2E") != "1" {
		t.Skip("set HELMR_NETWORK_E2E=1 on a privileged Linux host")
	}
	if os.Geteuid() != 0 {
		t.Fatal("privileged routed network test requires root")
	}
	stateDir := t.TempDir()
	connector := &Connector{
		cfg: Config{
			StateDir: stateDir, NetworkLinkPool: "198.18.0.0/29",
			NetworkTranslationPool: "198.19.0.0/30", NetworkResolverIPv4: "1.1.1.1",
			NetworkCapacity: 2, IPPath: "ip", NFTPath: "nft",
			JailerUID: 65534, JailerGID: 65534,
		},
		datapath: datapath.NewManager(),
	}
	if err := connector.datapath.VerifyKernel(); err != nil {
		t.Fatal(err)
	}
	owner := vm.Owner{Kind: vm.OwnerRuntime, ID: uuid.NewV7().String()}
	statePath := filepath.Join(stateDir, owner.ID)
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "owner"), []byte(string(owner.Kind)+"\n"+owner.ID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binding, err := connector.prepareNetworkBinding(context.Background(), owner, vm.WorkloadBinding{
		WorkerEpoch: 4, OwnerID: owner.ID, Generation: 1,
		RuntimeInstanceID: owner.ID, RuntimeIdentityID: runtimeid.Contract,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := binding.verify(true); err != nil {
		t.Fatal(err)
	}
	rootVeth, err := netlink.LinkByName(binding.manifest.RootVethName)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetMTU(rootVeth, GuestMTUV0-1); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-binding.Failure():
		if err == nil {
			t.Fatal("drift monitor reported a nil failure")
		}
	case <-time.After(2 * datapathRescanInterval):
		t.Fatal("drift monitor did not fence the attachment")
	}
	rootVeth, err = netlink.LinkByName(binding.manifest.RootVethName)
	if err != nil {
		t.Fatal(err)
	}
	if rootVeth.Attrs().Flags&net.FlagUp != 0 {
		t.Fatal("drifted root veth was not fenced")
	}
	if err := netlink.LinkSetMTU(rootVeth, GuestMTUV0); err != nil {
		t.Fatal(err)
	}
	if err := binding.Close(); err != nil {
		t.Fatal(err)
	}
	if err := connector.cleanupNetworkAttachment(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	if err := removeStateRootLast(statePath, owner); err != nil {
		t.Fatal(err)
	}
	if exists, err := connector.runtimeNetNSExists(context.Background(), owner.ID); err != nil || exists {
		t.Fatalf("namespace remains: exists=%t err=%v", exists, err)
	}
	if _, err := netlink.LinkByName(binding.manifest.RootVethName); err == nil {
		t.Fatal("root veth remains")
	} else if _, ok := err.(netlink.LinkNotFoundError); !ok {
		t.Fatal(err)
	}
}

func TestNetworkPoolRouteConflictTreatsDefaultAsFallback(t *testing.T) {
	pool := netip.MustParsePrefix("198.18.0.0/24")
	if networkPoolRouteConflict(pool, netip.MustParsePrefix("0.0.0.0/0")) {
		t.Fatal("IPv4 default route was treated as a pool conflict")
	}
	if !networkPoolRouteConflict(pool, netip.MustParsePrefix("198.18.0.0/15")) {
		t.Fatal("overlapping non-default route was not treated as a pool conflict")
	}
	if networkPoolRouteConflict(pool, netip.MustParsePrefix("203.0.113.0/24")) {
		t.Fatal("disjoint non-default route was treated as a pool conflict")
	}
}

func TestNetworkOwnerManifestIsExactAndAtomicallyReplaceable(t *testing.T) {
	connector := &Connector{cfg: Config{
		NetworkLinkPool: "198.18.0.0/29", NetworkTranslationPool: "198.19.0.0/30",
		NetworkResolverIPv4: "1.1.1.1", NetworkCapacity: 2,
	}}
	owner := vm.Owner{Kind: vm.OwnerRuntime, ID: "019c10d5-a6f7-7af1-8f5f-000000000021"}
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
		{Kind: vm.OwnerRuntime, ID: uuid.NewV7().String()},
		{Kind: vm.OwnerRuntime, ID: uuid.NewV7().String()},
	}
	for _, owner := range owners {
		if _, err := createOwnerStateRoot(stateDir, owner); err != nil {
			t.Fatal(err)
		}
	}
	binding := func(owner vm.Owner) vm.WorkloadBinding {
		return vm.WorkloadBinding{
			WorkerEpoch: 1, OwnerID: owner.ID, Generation: 1,
			RuntimeInstanceID: owner.ID, RuntimeIdentityID: runtimeid.Contract,
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
	owner := vm.Owner{Kind: vm.OwnerRuntime, ID: uuid.NewV7().String()}
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
		RuntimeInstanceID: owner.ID, RuntimeIdentityID: runtimeid.Contract,
	})
	if err == nil || !strings.Contains(err.Error(), "open network allocation lock") {
		t.Fatalf("error = %v", err)
	}
}

func TestWithNetworkBindingSurvivesSnapshotHandlerReplacement(t *testing.T) {
	connector := &Connector{
		cfg:      (Config{}).WithDefaults(),
		datapath: datapath.NewManager(),
	}
	logical := vm.WorkloadBinding{
		WorkerEpoch:       1,
		OwnerID:           "019c10d5-a6f7-7af1-8f5f-000000000020",
		Generation:        1,
		RuntimeInstanceID: "019c10d5-a6f7-7af1-8f5f-000000000020",
		RuntimeIdentityID: "runtime-identity",
	}
	var installed *installedNetworkBinding
	machine, err := firecracker.NewMachine(
		context.Background(),
		firecracker.Config{},
		firecracker.WithSnapshot("/tmp/mem", "/tmp/state"),
		connector.withNetworkBinding(
			vm.Owner{Kind: vm.OwnerRuntime, ID: logical.OwnerID},
			logical,
			&installed,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !machine.Handlers.FcInit.Has("helmr.InstallNetworkBinding") {
		t.Fatal("network binding handler was not installed after snapshot handlers")
	}
}
