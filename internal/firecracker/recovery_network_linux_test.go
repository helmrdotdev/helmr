//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/firecracker/datapath"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/helmrdotdev/helmr/internal/worker"
	"github.com/vishvananda/netlink"
)

// Uses the ordinary network installer and recovery path, with no VMM process.
// Command failures are injected at existing executable boundaries; replacement
// links are independently owned by this fixture, never authorized by the manifest.
func TestRecoveryRetainedNetworkPrivileged(t *testing.T) {
	if os.Getenv("HELMR_NETWORK_E2E") != "1" {
		t.Skip("requires an isolated privileged Linux network environment")
	}
	for _, failure := range []string{"root policy deletion", "namespace deletion", "replacement root veth"} {
		t.Run(failure, func(t *testing.T) {
			work, jailer := t.TempDir(), t.TempDir()
			stateDir := filepath.Join(work, "vms", "guest")
			ip, err := exec.LookPath("ip")
			if err != nil {
				t.Fatal(err)
			}
			nft, err := exec.LookPath("nft")
			if err != nil {
				t.Fatal(err)
			}
			fault := filepath.Join(t.TempDir(), "enabled")
			t.Setenv("HELMR_TEST_NETWORK_FAULT", fault)
			t.Setenv("HELMR_TEST_REAL_IP", ip)
			t.Setenv("HELMR_TEST_REAL_NFT", nft)
			wrapper := func(name, script string) string {
				path := filepath.Join(t.TempDir(), name)
				if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0700); err != nil {
					t.Fatal(err)
				}
				return path
			}
			cfg := Config{StateDir: stateDir, JailerChrootBaseDir: jailer,
				NetworkLinkPool: "198.18.0.0/29", NetworkTranslationPool: "198.19.0.0/30",
				NetworkResolverIPv4: "1.1.1.1", NetworkCapacity: 2,
				IPPath: ip, NFTPath: nft, JailerUID: 65534, JailerGID: 65534}
			if failure == "root policy deletion" {
				cfg.NFTPath = wrapper("nft", `if [ -e "$HELMR_TEST_NETWORK_FAULT" ] && [ "$1" = delete ]; then echo injected-root-policy-failure >&2; exit 33; fi
exec "$HELMR_TEST_REAL_NFT" "$@"
`)
			}
			if failure == "namespace deletion" {
				cfg.IPPath = wrapper("ip", `if [ -e "$HELMR_TEST_NETWORK_FAULT" ] && [ "$1 $2" = "netns delete" ]; then echo injected-namespace-failure >&2; exit 34; fi
exec "$HELMR_TEST_REAL_IP" "$@"
`)
			}
			connector := &Connector{cfg: cfg, datapath: datapath.NewManager()}
			if err := connector.datapath.VerifyKernel(); err != nil {
				t.Fatal(err)
			}
			owner := vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
			state, err := createOwnerStateRoot(stateDir, owner)
			if err != nil {
				t.Fatal(err)
			}
			jail := filepath.Join(jailer, "firecracker", owner.ID)
			if err := os.MkdirAll(jail, 0700); err != nil {
				t.Fatal(err)
			}
			binding, err := connector.prepareNetworkBinding(t.Context(), startupProbeLaunch, owner,
				vm.WorkloadBinding{WorkerEpoch: 1, OwnerID: owner.ID, Generation: 1, ComputerInstanceID: owner.ID, VMPlatformID: vmplatform.Contract})
			if err != nil {
				t.Fatal(err)
			}
			// Stop the live binding monitor before modeling process-absent residue.
			if err := binding.Close(); err != nil {
				t.Fatal(err)
			}
			var replacement netlink.Link
			t.Cleanup(func() {
				_ = os.Remove(fault)
				if replacement != nil {
					if current, err := netlink.LinkByName(replacement.Attrs().Name); err == nil && current.Attrs().Index == replacement.Attrs().Index {
						if err := netlink.LinkDel(current); err != nil {
							t.Error(err)
						}
					}
				}
				if err := connector.cleanupNetworkAttachment(context.Background(), owner); err != nil {
					t.Error(err)
				}
			})
			manifestPath := filepath.Join(state, networkManifestName)
			manifest, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "replacement root veth" {
				original, err := netlink.LinkByName(binding.manifest.RootVethName)
				if err != nil {
					t.Fatal(err)
				}
				if err := netlink.LinkDel(original); err != nil {
					t.Fatal(err)
				}
				link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: binding.manifest.RootVethName, MTU: GuestMTUV0}, PeerName: "p" + owner.ID[:8]}
				if err := netlink.LinkAdd(link); err != nil {
					t.Fatal(err)
				}
				replacement, err = netlink.LinkByName(link.Name)
				if err != nil {
					t.Fatal(err)
				}
				addr, err := netlink.ParseAddr(binding.manifest.RootIPv4CIDR)
				if err != nil {
					t.Fatal(err)
				}
				if err := netlink.AddrAdd(replacement, addr); err != nil {
					t.Fatal(err)
				}
				if err := netlink.LinkSetUp(replacement); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(fault, nil, 0600); err != nil {
				t.Fatal(err)
			}
			reclaimer, err := NewNetworkReclaimer(cfg)
			if err != nil {
				t.Fatal(err)
			}
			recover := func() worker.RecoveryEvidence {
				t.Helper()
				evidence, err := worker.RecoverLocalVMState(t.Context(), work, jailer, cfg.IPPath, reclaimer.Reclaim,
					func(owner vm.Owner) error { return RemoveStoppedCgroup(stateDir, "2", owner) })
				if err != nil {
					t.Fatal(err)
				}
				return evidence
			}
			for range 2 {
				evidence := recover()
				if len(evidence.Reclaimed) != 0 || !reflect.DeepEqual(evidence.QuarantinedOwners, []vm.Owner{owner}) || len(evidence.QuarantineErrors) != 1 {
					t.Fatalf("failed cleanup lost quarantine: %+v", evidence)
				}
				if !strings.Contains(evidence.QuarantineErrors[0], "reclaim exact network attachment") {
					t.Fatal(evidence.QuarantineErrors)
				}
				if failure == "namespace deletion" && !strings.Contains(evidence.QuarantineErrors[0], "injected-namespace-failure") {
					t.Fatalf("lost namespace cleanup diagnostic: %v", evidence.QuarantineErrors)
				}
				retained, err := os.ReadFile(manifestPath)
				if err != nil || !bytes.Equal(retained, manifest) {
					t.Fatalf("changed retained manifest: %v", err)
				}
				for _, path := range []string{filepath.Join(state, "owner"), jail} {
					if _, err := os.Lstat(path); err != nil {
						t.Fatalf("removed custody before cleanup: %v", err)
					}
				}
				exists, err := connector.runtimeNetNSExists(t.Context(), owner.ID)
				if err != nil || !exists {
					t.Fatalf("lost retained namespace: %v %v", exists, err)
				}
				_, rootErr := netlink.LinkByName(binding.manifest.RootVethName)
				overlap := rejectLiveNetworkOverlap(netip.MustParsePrefix(cfg.NetworkLinkPool), netip.MustParsePrefix(cfg.NetworkTranslationPool))
				if failure == "namespace deletion" {
					if _, absent := rootErr.(netlink.LinkNotFoundError); !absent || overlap != nil {
						t.Fatalf("partial cleanup root=%v overlap=%v", rootErr, overlap)
					}
				} else if rootErr != nil || overlap == nil {
					t.Fatalf("retained root did not block qualification: %v %v", rootErr, overlap)
				}
				if replacement != nil {
					current, err := netlink.LinkByName(replacement.Attrs().Name)
					if err != nil || current.Attrs().Index != replacement.Attrs().Index {
						t.Fatalf("deleted replacement: %v", err)
					}
				}
			}
			other := vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
			otherState, err := createOwnerStateRoot(stateDir, other)
			if err != nil {
				t.Fatal(err)
			}
			logical := vm.WorkloadBinding{WorkerEpoch: 1, OwnerID: other.ID, Generation: 1, ComputerInstanceID: other.ID, VMPlatformID: vmplatform.Contract}
			allocated, err := connector.allocateNetworkOwner(other, logical)
			if err != nil || allocated.AllocationIndex == binding.manifest.AllocationIndex {
				t.Fatalf("reused retained address index: %+v %v", allocated, err)
			}
			if err := removeStateRootLast(otherState, other); err != nil {
				t.Fatal(err)
			}
			// The fixture owns the replacement from LinkAdd above. This is explicit
			// operator reconciliation, separate from manifest-based recovery.
			if replacement != nil {
				if err := netlink.LinkDel(replacement); err != nil {
					t.Fatal(err)
				}
				replacement = nil
			} else if err := os.Remove(fault); err != nil {
				t.Fatal(err)
			}
			evidence := recover()
			if len(evidence.Quarantined) != 0 || !reflect.DeepEqual(evidence.Reclaimed, []string{owner.ID}) {
				t.Fatalf("ordinary retry failed: %+v", evidence)
			}
			for _, path := range []string{state, jail} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("custody remains after recovery: %s: %v", path, err)
				}
			}
			if err := rejectLiveNetworkOverlap(netip.MustParsePrefix(cfg.NetworkLinkPool), netip.MustParsePrefix(cfg.NetworkTranslationPool)); err != nil {
				t.Fatal(err)
			}
			other = vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
			logical.OwnerID, logical.ComputerInstanceID = other.ID, other.ID
			otherState = filepath.Join(stateDir, other.ID)
			if _, err := createOwnerStateRoot(stateDir, other); err != nil {
				t.Fatal(err)
			}
			allocated, err = connector.allocateNetworkOwner(other, logical)
			if err != nil || allocated.AllocationIndex != binding.manifest.AllocationIndex {
				t.Fatalf("recovered address not reusable: %+v %v", allocated, err)
			}
			if err := removeStateRootLast(otherState, other); err != nil {
				t.Fatal(err)
			}
		})
	}
}
