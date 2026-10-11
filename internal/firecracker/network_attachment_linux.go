//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/helmrdotdev/helmr/internal/firecracker/datapath"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func (c *Connector) createRoutedAttachment(ctx context.Context, binding *installedNetworkBinding) error {
	m := binding.manifest
	if err := exec.CommandContext(ctx, c.cfg.IPPath, "netns", "add", m.NamespaceName).Run(); err != nil {
		return fmt.Errorf("create routed network namespace: %w", err)
	}
	namespace, err := netns.GetFromName(m.NamespaceName)
	if err != nil {
		return fmt.Errorf("open routed network namespace: %w", err)
	}
	binding.namespace = namespace
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: m.RootVethName, MTU: GuestMTUV0}, PeerName: m.NamespaceVethName, PeerMTU: GuestMTUV0}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("create routed veth pair: %w", err)
	}
	rootVeth, err := netlink.LinkByName(m.RootVethName)
	if err != nil {
		return fmt.Errorf("find root routed veth: %w", err)
	}
	binding.rootIfindex = rootVeth.Attrs().Index
	peer, err := netlink.LinkByName(m.NamespaceVethName)
	if err != nil {
		return fmt.Errorf("find namespace routed veth: %w", err)
	}
	if err := netlink.LinkSetNsFd(peer, int(namespace)); err != nil {
		return fmt.Errorf("move routed veth into namespace: %w", err)
	}
	nsHandle, err := netlink.NewHandleAt(namespace)
	if err != nil {
		return fmt.Errorf("open namespace netlink handle: %w", err)
	}
	defer nsHandle.Close()
	nsVeth, err := nsHandle.LinkByName(m.NamespaceVethName)
	if err != nil {
		return fmt.Errorf("find moved namespace veth: %w", err)
	}
	binding.namespaceIfindex = nsVeth.Attrs().Index
	gatewayMAC, _ := net.ParseMAC(GuestGatewayMACV0)
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: m.TapName, MTU: GuestMTUV0, HardwareAddr: gatewayMAC},
		Mode:      netlink.TUNTAP_MODE_TAP, Flags: netlink.TUNTAP_DEFAULTS | netlink.TUNTAP_VNET_HDR,
		Owner: uint32(c.cfg.JailerUID), Group: uint32(c.cfg.JailerGID),
	}
	if err := withNetworkNamespace(namespace, func() error { return netlink.LinkAdd(tap) }); err != nil {
		return fmt.Errorf("create routed TAP: %w", err)
	}
	tap, err = linkAsTuntap(nsHandle, m.TapName)
	if err != nil {
		return err
	}
	if err := nsHandle.LinkSetMTU(tap, GuestMTUV0); err != nil {
		return fmt.Errorf("set routed TAP MTU: %w", err)
	}
	if err := nsHandle.LinkSetHardwareAddr(tap, gatewayMAC); err != nil {
		return fmt.Errorf("set routed TAP gateway MAC: %w", err)
	}
	tap, err = linkAsTuntap(nsHandle, m.TapName)
	if err != nil {
		return err
	}
	binding.tapIfindex = tap.Attrs().Index
	rootAddr, _ := netlink.ParseAddr(m.RootIPv4CIDR)
	if err := netlink.AddrAdd(rootVeth, rootAddr); err != nil {
		return fmt.Errorf("assign root veth address: %w", err)
	}
	nsAddr, _ := netlink.ParseAddr(m.NamespaceIPv4CIDR)
	if err := nsHandle.AddrAdd(nsVeth, nsAddr); err != nil {
		return fmt.Errorf("assign namespace veth address: %w", err)
	}
	guestGateway, _ := netlink.ParseAddr(GuestGatewayIPv4V0 + "/30")
	if err := nsHandle.AddrAdd(tap, guestGateway); err != nil {
		return fmt.Errorf("assign routed TAP gateway: %w", err)
	}
	loopback, err := nsHandle.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("find namespace loopback: %w", err)
	}
	translationAddr, _ := netlink.ParseAddr(m.TranslationIPv4CIDR)
	if err := nsHandle.AddrAdd(loopback, translationAddr); err != nil {
		return fmt.Errorf("assign translation identity: %w", err)
	}
	if err := nsHandle.LinkSetUp(loopback); err != nil {
		return fmt.Errorf("raise namespace loopback: %w", err)
	}
	if err := withNetworkNamespace(namespace, func() error {
		for path, value := range map[string]string{
			"/proc/sys/net/ipv4/ip_forward":             "1\n",
			"/proc/sys/net/ipv4/conf/all/rp_filter":     "0\n",
			"/proc/sys/net/ipv4/conf/default/rp_filter": "0\n",
		} {
			if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
				return fmt.Errorf("configure namespace sysctl %s: %w", path, err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func linkAsTuntap(handle *netlink.Handle, name string) (*netlink.Tuntap, error) {
	link, err := handle.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("find routed TAP: %w", err)
	}
	tap, ok := link.(*netlink.Tuntap)
	if !ok {
		return nil, errors.New("routed TAP has unexpected link type")
	}
	return tap, nil
}

func (c *Connector) cleanupNetworkAttachment(ctx context.Context, owner vm.Owner) error {
	path := filepath.Join(c.cfg.StateDir, owner.ID, networkManifestName)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read network owner manifest: %w", err)
	}
	var manifest networkOwnerManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("decode network owner manifest: %w", err)
	}
	if err := c.validateNetworkOwnerManifest(manifest, owner); err != nil {
		return fmt.Errorf("validate exact network owner manifest: %w", err)
	}
	if rootVeth, findErr := netlink.LinkByName(manifest.RootVethName); findErr == nil {
		if manifest.Installed && (rootVeth.Type() != "veth" || rootVeth.Attrs().Index != manifest.RootIfindex || rootVeth.Attrs().MTU != manifest.MTU) {
			return errors.New("refusing to clean replacement root veth")
		}
		if err := netlink.LinkSetDown(rootVeth); err != nil {
			return fmt.Errorf("fence root veth during cleanup: %w", err)
		}
	} else if _, ok := findErr.(netlink.LinkNotFoundError); !ok {
		return fmt.Errorf("find root veth during cleanup: %w", findErr)
	}
	if manifest.Installed {
		fingerprint, err := c.nftTableFingerprint(ctx, "", manifest.RootTableName)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && fingerprint != manifest.RootPolicyHash {
			return errors.New("refusing to clean replacement root policy")
		}
	}
	if err := c.deleteNftTable(ctx, "", manifest.RootTableName); err != nil {
		return err
	}
	if rootVeth, findErr := netlink.LinkByName(manifest.RootVethName); findErr == nil {
		if err := netlink.LinkDel(rootVeth); err != nil {
			return fmt.Errorf("delete root veth: %w", err)
		}
	}
	if exists, err := c.runtimeNetNSExists(ctx, manifest.NamespaceName); err != nil {
		return err
	} else if exists {
		namespace, err := netns.GetFromName(manifest.NamespaceName)
		if err != nil {
			return fmt.Errorf("open exact cleanup namespace: %w", err)
		}
		nsHandle, err := netlink.NewHandleAt(namespace)
		if err != nil {
			_ = namespace.Close()
			return fmt.Errorf("open exact cleanup netlink handle: %w", err)
		}
		if manifest.Installed {
			if nsVeth, findErr := nsHandle.LinkByName(manifest.NamespaceVethName); findErr == nil {
				if nsVeth.Type() != "veth" || nsVeth.Attrs().Index != manifest.NamespaceIfindex || nsVeth.Attrs().MTU != manifest.MTU {
					nsHandle.Close()
					namespace.Close()
					return errors.New("refusing to clean replacement namespace veth")
				}
			} else if _, ok := findErr.(netlink.LinkNotFoundError); !ok {
				nsHandle.Close()
				namespace.Close()
				return fmt.Errorf("find namespace veth during cleanup: %w", findErr)
			}
			tapLink, findErr := nsHandle.LinkByName(manifest.TapName)
			if findErr == nil {
				tap, ok := tapLink.(*netlink.Tuntap)
				if !ok {
					nsHandle.Close()
					namespace.Close()
					return errors.New("refusing to clean replacement non-TAP link")
				}
				if tap.Attrs().Index != manifest.TapIfindex || tap.Attrs().MTU != manifest.MTU ||
					tap.Mode != netlink.TUNTAP_MODE_TAP || tap.Flags&netlink.TUNTAP_VNET_HDR == 0 ||
					tap.Owner != uint32(c.cfg.JailerUID) || tap.Group != uint32(c.cfg.JailerGID) ||
					tap.Attrs().HardwareAddr.String() != manifest.GatewayMAC {
					nsHandle.Close()
					namespace.Close()
					return errors.New("refusing to clean replacement routed TAP")
				}
				identity := datapath.BindingIdentity{
					Ifindex: manifest.TapIfindex, ProgramID: manifest.BPFProgramID,
					ProgramTag: manifest.BPFProgramTag, FilterHandle: manifest.BPFFilterHandle,
					Mark: manifest.PacketMark,
				}
				if err := withNetworkNamespace(namespace, func() error { return datapath.DetachExact(manifest.TapName, identity) }); err != nil {
					nsHandle.Close()
					namespace.Close()
					return fmt.Errorf("detach exact persisted ingress binding: %w", err)
				}
			} else if _, ok := findErr.(netlink.LinkNotFoundError); !ok {
				nsHandle.Close()
				namespace.Close()
				return fmt.Errorf("find routed TAP during cleanup: %w", findErr)
			}
			fingerprint, err := c.nftTableFingerprint(ctx, manifest.NamespaceName, networkPolicyTableName)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				nsHandle.Close()
				namespace.Close()
				return err
			}
			if err == nil && fingerprint != manifest.NamespacePolicyHash {
				nsHandle.Close()
				namespace.Close()
				return errors.New("refusing to clean replacement namespace policy")
			}
		}
		nsHandle.Close()
		namespace.Close()
		if err := c.deleteNftTable(ctx, manifest.NamespaceName, networkPolicyTableName); err != nil {
			return err
		}
		if output, err := exec.CommandContext(ctx, c.cfg.IPPath, "netns", "delete", manifest.NamespaceName).CombinedOutput(); err != nil {
			return fmt.Errorf("delete routed network namespace: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	if _, err := netlink.LinkByName(manifest.RootVethName); err == nil {
		return errors.New("root veth remains after cleanup")
	} else if _, ok := err.(netlink.LinkNotFoundError); !ok {
		return fmt.Errorf("prove root veth absence: %w", err)
	}
	if exists, err := c.runtimeNetNSExists(ctx, manifest.NamespaceName); err != nil {
		return fmt.Errorf("prove routed namespace absence: %w", err)
	} else if exists {
		return errors.New("routed namespace remains after cleanup")
	}
	if _, err := c.nftTableFingerprint(ctx, "", manifest.RootTableName); err == nil {
		return errors.New("root policy remains after cleanup")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("prove root policy absence: %w", err)
	}
	return nil
}

// A failed restore must retire the contaminated OS thread. The dedicated
// goroutine returns while locked in that case, causing Go to destroy its thread.
func withNetworkNamespace(target netns.NsHandle, fn func() error) error {
	return withNetworkNamespaceRestore(target, fn, netns.Set)
}

func withNetworkNamespaceRestore(target netns.NsHandle, fn func() error, restore func(netns.NsHandle) error) error {
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		current, err := netns.Get()
		if err != nil {
			runtime.UnlockOSThread()
			result <- fmt.Errorf("open current network namespace: %w", err)
			return
		}
		defer current.Close()
		if err := netns.Set(target); err != nil {
			runtime.UnlockOSThread()
			result <- fmt.Errorf("enter network namespace: %w", err)
			return
		}
		callErr := fn()
		restoreErr := restore(current)
		if restoreErr == nil {
			runtime.UnlockOSThread()
		} else {
			restoreErr = fmt.Errorf("restore network namespace: %w", restoreErr)
		}
		result <- errors.Join(callErr, restoreErr)
	}()
	return <-result
}

func staticNetworkInterface() firecracker.NetworkInterface {
	return firecracker.NetworkInterface{StaticConfiguration: &firecracker.StaticNetworkConfiguration{
		HostDevName: GuestTapNameV0,
		MacAddress:  GuestMACV0,
	}}
}
