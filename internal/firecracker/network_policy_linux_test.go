//go:build linux

package firecracker

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/firecracker/datapath"
	"github.com/helmrdotdev/helmr/internal/secretproxy"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
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
			// This fixture has no protected bindings; production preparation returns nil.
			PrepareSecretTransport: func(context.Context, string, []netip.Prefix) (*secretproxy.Proxy, error) { return nil, nil },
		},
		datapath: datapath.NewManager(),
	}
	if err := connector.datapath.VerifyKernel(); err != nil {
		t.Fatal(err)
	}
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
	statePath := filepath.Join(stateDir, owner.ID)
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "owner"), []byte(string(owner.Kind)+"\n"+owner.ID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binding, err := connector.prepareNetworkBinding(context.Background(), workloadLaunch, owner, vm.WorkloadBinding{
		WorkerEpoch: 4, OwnerID: owner.ID, Generation: 1,
		ComputerInstanceID: owner.ID, VMPlatformID: vmplatform.Contract,
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
