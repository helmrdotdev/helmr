//go:build linux && computerproof

package controlplane

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/vishvananda/netlink"
)

type lossNetworkFault struct {
	mode, enabled, state, jail, owner, root string
	replacementIndex                        int
	allocationIndex                         uint32
	manifest, marker                        []byte
}

// The fault is only an executable wrapper in this disposable fixture. Production
// binaries receive no new flags or fault injection surface.
func installLossNetworkFault(t *testing.T, input nativeExecutionConfig, environment map[string]string, owner, mode string) *lossNetworkFault {
	t.Helper()
	f := &lossNetworkFault{mode: mode, owner: owner,
		enabled: filepath.Join(input.Evidence, "network-fault"),
		state:   filepath.Join(environment["WORKER_WORK_DIR"], "vms", "guest", owner),
		jail:    filepath.Join(environment["JAILER_CHROOT_DIR"], "firecracker", owner)}
	var err error
	f.manifest, err = os.ReadFile(filepath.Join(f.state, "network.json"))
	if err != nil {
		t.Fatal(err)
	}
	f.marker, err = os.ReadFile(filepath.Join(f.state, "owner"))
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		RootVethName    string `json:"root_veth_name"`
		RootTableName   string `json:"root_table_name"`
		RootIfindex     int    `json:"root_ifindex"`
		RootIPv4CIDR    string `json:"root_ipv4_cidr"`
		MTU             int    `json:"mtu"`
		AllocationIndex uint32 `json:"allocation_index"`
	}
	if err := json.Unmarshal(f.manifest, &identity); err != nil || identity.RootVethName == "" {
		t.Fatalf("network manifest: %v", err)
	}
	f.root = identity.RootVethName
	f.allocationIndex = identity.AllocationIndex
	if mode == "replacement-root" {
		original, err := netlink.LinkByName(f.root)
		if err != nil || original.Type() != "veth" || original.Attrs().Index != identity.RootIfindex || original.Attrs().MTU != identity.MTU {
			t.Fatalf("fixture root identity changed before replacement: %v", err)
		}
		if err := netlink.LinkDel(original); err != nil {
			t.Fatal(err)
		}
		link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: f.root, MTU: identity.MTU}, PeerName: "p" + owner[:8]}
		if err := netlink.LinkAdd(link); err != nil {
			t.Fatal(err)
		}
		replacement, err := netlink.LinkByName(f.root)
		if err != nil {
			t.Fatal(err)
		}
		f.replacementIndex = replacement.Attrs().Index
		addr, err := netlink.ParseAddr(identity.RootIPv4CIDR)
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.AddrAdd(replacement, addr); err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetUp(replacement); err != nil {
			t.Fatal(err)
		}
		receipt, err := json.Marshal(map[string]any{"owner": owner, "root": f.root, "originalIndex": identity.RootIfindex, "replacementIndex": f.replacementIndex})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(input.Evidence, "network-replacement.json"), receipt, 0600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	key, fallback, condition := "NFT_PATH", "nft", `[ "$1" = delete ] && [ "$4" = "$HELMR_TEST_NETWORK_TARGET" ]`
	if mode == "namespace" {
		key, fallback, condition = "IP_PATH", "ip", `[ "$1 $2" = "netns delete" ] && [ "$3" = "$HELMR_TEST_NETWORK_TARGET" ]`
	}
	environment["HELMR_TEST_NETWORK_TARGET"] = identity.RootTableName
	if mode == "namespace" {
		environment["HELMR_TEST_NETWORK_TARGET"] = owner
	}
	realPath := environment[key]
	if realPath == "" {
		realPath = fallback
	}
	realPath, err = exec.LookPath(realPath)
	if err != nil {
		t.Fatal(err)
	}
	environment["HELMR_TEST_NETWORK_TOOL"] = realPath
	environment["HELMR_TEST_NETWORK_FAULT"] = f.enabled
	wrapper := filepath.Join(input.Evidence, "network-tool")
	script := "#!/bin/sh\nif [ -e \"$HELMR_TEST_NETWORK_FAULT\" ] && " + condition + "; then echo injected-network-cleanup-failure >&2; exit 33; fi\nexec \"$HELMR_TEST_NETWORK_TOOL\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.enabled, nil, 0600); err != nil {
		t.Fatal(err)
	}
	environment[key] = wrapper
	return f
}

func (f *lossNetworkFault) assertRetained(t *testing.T, log []byte) {
	t.Helper()
	for path, want := range map[string][]byte{filepath.Join(f.state, "network.json"): f.manifest, filepath.Join(f.state, "owner"): f.marker} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("lost retained network evidence: %s: %v", path, err)
		}
	}
	if _, err := os.Stat(f.jail); err != nil {
		t.Fatalf("lost retained jail: %v", err)
	}
	if !bytes.Contains(log, []byte(f.owner)) || !bytes.Contains(log, []byte("reclaim exact network attachment")) {
		t.Fatal("Worker log lost owner-specific network cleanup failure")
	}
	link, err := netlink.LinkByName(f.root)
	if f.mode != "namespace" {
		if err != nil {
			t.Fatalf("root removed before injected failure: %v", err)
		}
		if f.mode == "replacement-root" && (link.Attrs().Index != f.replacementIndex || !bytes.Contains(log, []byte("refusing to clean replacement root veth"))) {
			t.Fatal("replacement identity or refusal lost")
		}
	} else if _, absent := err.(netlink.LinkNotFoundError); !absent {
		t.Fatalf("root not removed before namespace failure: %v", err)
	}
}

func (f *lossNetworkFault) clear(t *testing.T) {
	t.Helper()
	if f.mode == "replacement-root" {
		link, err := netlink.LinkByName(f.root)
		if err != nil || link.Attrs().Index != f.replacementIndex {
			t.Fatalf("fixture replacement identity changed: expected index=%d error=%v", f.replacementIndex, err)
		}
		if err := netlink.LinkDel(link); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.Remove(f.enabled); err != nil {
		t.Fatal(err)
	}
}
