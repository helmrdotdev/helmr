//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/worker/datapath"
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
	owner := vm.Owner{Kind: vm.OwnerRuntime, ID: uuid.Must(uuid.NewV7()).String()}
	statePath := filepath.Join(stateDir, owner.ID)
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "owner"), []byte(string(owner.Kind)+"\n"+owner.ID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binding, err := connector.prepareNetworkBinding(context.Background(), owner, vm.WorkloadBinding{
		WorkerEpoch: 4, OwnerID: owner.ID, Generation: 1,
		RuntimeInstanceID: owner.ID, RuntimeIdentityID: NetworkABIV0,
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

func TestParseBuildNetworkStatusRequiresBothCounters(t *testing.T) {
	status, err := parseBuildNetworkStatus([]byte(`{
		"nftables": [
			{"metainfo": {"json_schema_version": 1}},
			{"counter": {"family": "inet", "name": "build_denied", "table": "helmr_network_policy", "packets": 2, "bytes": 120}},
			{"counter": {"family": "inet", "name": "build_limit", "table": "helmr_network_policy", "packets": 3, "bytes": 180}}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if status.DeniedPackets != 2 || status.LimitPackets != 3 {
		t.Fatalf("build network status = %+v", status)
	}
	if _, err := parseBuildNetworkStatus([]byte(`{
		"nftables": [
			{"counter": {"name": "build_denied", "packets": 1}}
		]
	}`)); err == nil {
		t.Fatal("incomplete build network counters were accepted")
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
		connector.withNetworkBinding(logical, &installed),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !machine.Handlers.FcInit.Has("helmr.InstallNetworkBinding") {
		t.Fatal("network binding handler was not installed after snapshot handlers")
	}
}
