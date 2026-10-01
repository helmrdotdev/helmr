//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/helmrdotdev/helmr/internal/firecracker/datapath"
	"github.com/helmrdotdev/helmr/internal/secretproxy"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const (
	datapathRescanInterval = 5 * time.Second
	networkManifestName    = "network.json"
	networkManifestVersion = "helmr.network-owner.v0"
	namespaceVethName      = "host0"
)

type installedNetworkBinding struct {
	secretProxy                *secretproxy.Proxy
	connector                  *Connector
	manifest                   networkOwnerManifest
	packet                     *datapath.Binding
	namespace                  netns.NsHandle
	rootIfindex                int
	namespaceIfindex           int
	tapIfindex                 int
	namespacePolicyFingerprint string
	rootPolicyFingerprint      string
	failure                    chan error
	stop                       chan struct{}
	done                       chan struct{}
	stopOnce                   sync.Once
	active                     bool
	mu                         sync.Mutex
}

func (c *Connector) withNetworkBinding(
	mode launchMode,
	owner vm.Owner,
	logical vm.WorkloadBinding,
	installed **installedNetworkBinding,
) firecracker.Opt {
	return func(sdkMachine *firecracker.Machine) {
		sdkMachine.Handlers.FcInit = sdkMachine.Handlers.FcInit.Prepend(firecracker.Handler{
			Name: "helmr.InstallNetworkBinding",
			Fn: func(ctx context.Context, _ *firecracker.Machine) error {
				binding, err := c.prepareNetworkBinding(ctx, mode, owner, logical)
				if err != nil {
					return err
				}
				*installed = binding
				return nil
			},
		})
	}
}

func (c *Connector) prepareNetworkBinding(
	ctx context.Context,
	mode launchMode,
	owner vm.Owner,
	logical vm.WorkloadBinding,
) (_ *installedNetworkBinding, returnErr error) {
	if err := logical.Validate(owner); err != nil {
		return nil, fmt.Errorf("validate logical datapath binding: %w", err)
	}
	manifest, err := c.allocateNetworkOwner(owner, logical)
	if err != nil {
		return nil, err
	}
	binding := &installedNetworkBinding{
		connector: c,
		manifest:  manifest,
		namespace: netns.None(),
		failure:   make(chan error, 1),
		stop:      make(chan struct{}),
	}
	defer func() {
		if returnErr != nil {
			closeErr := binding.Close()
			if closeErr != nil {
				returnErr = errors.Join(returnErr, closeErr)
				return
			}
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			cleanupErr := c.cleanupNetworkAttachment(cleanupCtx, owner)
			cancel()
			returnErr = errors.Join(returnErr, closeErr, cleanupErr)
		}
	}()
	if err := c.createRoutedAttachment(ctx, binding); err != nil {
		return nil, err
	}
	if err := c.installRoutedPolicy(ctx, mode, binding); err != nil {
		return nil, err
	}
	if err := c.persistInstalledNetworkOwner(binding); err != nil {
		return nil, err
	}
	if err := binding.verify(false); err != nil {
		return nil, fmt.Errorf("verify inactive routed attachment: %w", err)
	}
	binding.done = make(chan struct{})
	go binding.monitor()
	if err := binding.activate(); err != nil {
		return nil, err
	}
	return binding, nil
}

func (binding *installedNetworkBinding) activate() error {
	binding.mu.Lock()
	defer binding.mu.Unlock()
	if binding.active {
		return errors.New("routed network binding is already active")
	}
	if err := withNetworkNamespace(binding.namespace, binding.packet.Activate); err != nil {
		return fmt.Errorf("activate TAP ingress binding: %w", err)
	}
	nsHandle, err := netlink.NewHandleAt(binding.namespace)
	if err != nil {
		return fmt.Errorf("open namespace for activation: %w", err)
	}
	defer nsHandle.Close()
	nsVeth, err := nsHandle.LinkByName(binding.manifest.NamespaceVethName)
	if err != nil {
		return err
	}
	tap, err := nsHandle.LinkByName(binding.manifest.TapName)
	if err != nil {
		return err
	}
	rootVeth, err := netlink.LinkByName(binding.manifest.RootVethName)
	if err != nil {
		return err
	}
	if err := nsHandle.LinkSetUp(nsVeth); err != nil {
		return fmt.Errorf("raise namespace veth: %w", err)
	}
	if err := netlink.LinkSetUp(rootVeth); err != nil {
		return fmt.Errorf("raise root veth: %w", err)
	}
	if err := installRoutedRoutes(binding); err != nil {
		_ = netlink.LinkSetDown(rootVeth)
		return err
	}
	guestMAC, _ := net.ParseMAC(binding.manifest.GuestMAC)
	guestIP := net.ParseIP(netip.MustParsePrefix(binding.manifest.GuestIPv4CIDR).Addr().String()).To4()
	if err := nsHandle.NeighAdd(&netlink.Neigh{
		LinkIndex: tap.Attrs().Index, IP: guestIP,
		HardwareAddr: guestMAC, State: netlink.NUD_PERMANENT,
	}); err != nil {
		_ = netlink.LinkSetDown(rootVeth)
		return fmt.Errorf("install exact guest neighbor: %w", err)
	}
	if err := nsHandle.LinkSetUp(tap); err != nil {
		return fmt.Errorf("raise routed TAP: %w", err)
	}
	binding.active = true
	if err := binding.verifyLocked(true); err != nil {
		_ = netlink.LinkSetDown(rootVeth)
		binding.active = false
		return fmt.Errorf("verify active routed attachment: %w", err)
	}
	return nil
}

func (binding *installedNetworkBinding) monitor() {
	defer close(binding.done)
	ticker := time.NewTicker(datapathRescanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-binding.stop:
			return
		case <-ticker.C:
			if err := binding.verify(true); err != nil {
				fenceErr := binding.fence()
				if binding.secretProxy != nil {
					fenceErr = errors.Join(fenceErr, binding.secretProxy.Close())
				}
				_ = binding.packet.Invalidate(err)
				select {
				case binding.failure <- errors.Join(err, fenceErr):
				default:
				}
				return
			}
		}
	}
}

func (binding *installedNetworkBinding) verify(expectUp bool) error {
	binding.mu.Lock()
	defer binding.mu.Unlock()
	return binding.verifyLocked(expectUp)
}

func (binding *installedNetworkBinding) verifyLocked(expectUp bool) error {
	if err := binding.connector.validateNetworkOwnerManifest(binding.manifest, vm.Owner{Kind: vm.OwnerKind(binding.manifest.OwnerKind), ID: binding.manifest.OwnerID}); err != nil {
		return fmt.Errorf("validate network owner authority: %w", err)
	}
	rootVeth, err := netlink.LinkByName(binding.manifest.RootVethName)
	if err != nil || rootVeth.Type() != "veth" || rootVeth.Attrs().Index != binding.rootIfindex || binding.rootIfindex != binding.manifest.RootIfindex || rootVeth.Attrs().MTU != binding.manifest.MTU {
		return errors.New("root veth identity changed")
	}
	if expectUp && rootVeth.Attrs().Flags&net.FlagUp == 0 {
		return errors.New("root veth is down")
	}
	if !expectUp && rootVeth.Attrs().Flags&net.FlagUp != 0 {
		return errors.New("inactive root veth is up")
	}
	nsHandle, err := netlink.NewHandleAt(binding.namespace)
	if err != nil {
		return err
	}
	defer nsHandle.Close()
	nsVeth, err := nsHandle.LinkByName(binding.manifest.NamespaceVethName)
	if err != nil || nsVeth.Type() != "veth" || nsVeth.Attrs().Index != binding.namespaceIfindex || binding.namespaceIfindex != binding.manifest.NamespaceIfindex || nsVeth.Attrs().MTU != binding.manifest.MTU {
		return errors.New("namespace veth identity changed")
	}
	tap, err := linkAsTuntap(nsHandle, binding.manifest.TapName)
	if err != nil {
		return fmt.Errorf("find exact routed TAP: %w", err)
	}
	if tap.Attrs().Index != binding.tapIfindex || binding.tapIfindex != binding.manifest.TapIfindex || tap.Attrs().MTU != binding.manifest.MTU ||
		tap.Mode != netlink.TUNTAP_MODE_TAP || tap.Flags&netlink.TUNTAP_VNET_HDR == 0 ||
		tap.Owner != uint32(binding.connector.cfg.JailerUID) || tap.Group != uint32(binding.connector.cfg.JailerGID) ||
		tap.Attrs().HardwareAddr.String() != binding.manifest.GatewayMAC {
		return fmt.Errorf("routed TAP identity changed: ifindex=%d/%d manifest=%d mtu=%d/%d mode=%d flags=%d owner=%d/%d group=%d/%d mac=%s/%s",
			tap.Attrs().Index, binding.tapIfindex, binding.manifest.TapIfindex,
			tap.Attrs().MTU, binding.manifest.MTU, tap.Mode, tap.Flags,
			tap.Owner, binding.connector.cfg.JailerUID, tap.Group, binding.connector.cfg.JailerGID,
			tap.Attrs().HardwareAddr, binding.manifest.GatewayMAC)
	}
	if expectUp && (nsVeth.Attrs().Flags&net.FlagUp == 0 || tap.Attrs().Flags&net.FlagUp == 0) {
		return errors.New("namespace attachment link is down")
	}
	if !expectUp && (nsVeth.Attrs().Flags&net.FlagUp != 0 || tap.Attrs().Flags&net.FlagUp != 0) {
		return errors.New("inactive namespace attachment link is up")
	}
	if err := verifyRoutedAddressesAndRoutes(nsHandle, rootVeth, nsVeth, tap, binding.manifest, expectUp); err != nil {
		return err
	}
	if binding.secretProxy != nil {
		if err := withNetworkNamespace(binding.namespace, func() error { return verifySecretRoute(binding.manifest.TapName, binding.manifest.PacketMark) }); err != nil {
			return err
		}
	}
	if err := withNetworkNamespace(binding.namespace, verifyRoutedNamespaceSysctls); err != nil {
		return err
	}
	if err := withNetworkNamespace(binding.namespace, binding.packet.Verify); err != nil {
		return err
	}
	identity, err := binding.packet.Identity()
	if err != nil || identity.Ifindex != binding.manifest.TapIfindex || identity.ProgramID != binding.manifest.BPFProgramID ||
		identity.ProgramTag != binding.manifest.BPFProgramTag || identity.FilterHandle != binding.manifest.BPFFilterHandle || identity.Mark != binding.manifest.PacketMark {
		return errors.New("TAP ingress classifier identity changed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	namespaceFingerprint, err := binding.connector.nftTableFingerprint(ctx, binding.manifest.NamespaceName, networkPolicyTableName)
	if err != nil || namespaceFingerprint != binding.namespacePolicyFingerprint || namespaceFingerprint != binding.manifest.NamespacePolicyHash {
		return errors.New("namespace network policy changed")
	}
	rootFingerprint, err := binding.connector.nftTableFingerprint(ctx, "", binding.manifest.RootTableName)
	if err != nil || rootFingerprint != binding.rootPolicyFingerprint || rootFingerprint != binding.manifest.RootPolicyHash {
		return errors.New("root network policy changed")
	}
	return nil
}

func verifyRoutedAddressesAndRoutes(handle *netlink.Handle, rootVeth, namespaceVeth netlink.Link, tap *netlink.Tuntap, manifest networkOwnerManifest, expectRoutes bool) error {
	if err := verifyExactIPv4Address(nil, rootVeth, manifest.RootIPv4CIDR); err != nil {
		return fmt.Errorf("verify root veth address: %w", err)
	}
	if err := verifyExactIPv4Address(handle, namespaceVeth, manifest.NamespaceIPv4CIDR); err != nil {
		return fmt.Errorf("verify namespace veth address: %w", err)
	}
	if err := verifyExactIPv4Address(handle, tap, manifest.GatewayIPv4+"/30"); err != nil {
		return fmt.Errorf("verify TAP gateway address: %w", err)
	}
	loopback, err := handle.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("find namespace loopback: %w", err)
	}
	addresses, err := handle.AddrList(loopback, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list translation addresses: %w", err)
	}
	if !slices.ContainsFunc(addresses, func(address netlink.Addr) bool {
		return address.IPNet != nil && address.IPNet.String() == manifest.TranslationIPv4CIDR
	}) {
		return errors.New("translation identity changed")
	}
	rootIP := net.ParseIP(netip.MustParsePrefix(manifest.RootIPv4CIDR).Addr().String()).To4()
	namespaceRoutes, err := handle.RouteList(namespaceVeth, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list namespace routes: %w", err)
	}
	defaultRoutes := 0
	for _, route := range namespaceRoutes {
		if isDefaultIPv4Route(route.Dst) {
			defaultRoutes++
			if expectRoutes && (route.LinkIndex != namespaceVeth.Attrs().Index || !route.Gw.Equal(rootIP) || route.Flags&int(unix.RTNH_F_ONLINK) == 0) {
				return errors.New("namespace default route changed")
			}
		}
	}
	expectedRouteCount := 0
	if expectRoutes {
		expectedRouteCount = 1
	}
	if defaultRoutes != expectedRouteCount {
		return fmt.Errorf("namespace default route count changed: %d, want %d", defaultRoutes, expectedRouteCount)
	}
	namespaceIP := net.ParseIP(netip.MustParsePrefix(manifest.NamespaceIPv4CIDR).Addr().String()).To4()
	translation := netip.MustParsePrefix(manifest.TranslationIPv4CIDR)
	rootRoutes, err := netlink.RouteList(rootVeth, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list root routes: %w", err)
	}
	translationRoutes := 0
	for _, route := range rootRoutes {
		prefix, ok := netipPrefix(route.Dst)
		if ok && prefix == translation {
			translationRoutes++
			if expectRoutes && (route.LinkIndex != rootVeth.Attrs().Index || !route.Gw.Equal(namespaceIP) || route.Flags&int(unix.RTNH_F_ONLINK) == 0) {
				return errors.New("translation route changed")
			}
		}
	}
	if translationRoutes != expectedRouteCount {
		return fmt.Errorf("translation route count changed: %d, want %d", translationRoutes, expectedRouteCount)
	}
	neighbors, err := handle.NeighList(tap.Attrs().Index, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list TAP neighbors: %w", err)
	}
	guestIP := net.ParseIP(netip.MustParsePrefix(manifest.GuestIPv4CIDR).Addr().String()).To4()
	guestMAC, _ := net.ParseMAC(manifest.GuestMAC)
	exactNeighbors := 0
	for _, neighbor := range neighbors {
		if neighbor.IP.Equal(guestIP) {
			exactNeighbors++
			if neighbor.LinkIndex != tap.Attrs().Index || !bytes.Equal(neighbor.HardwareAddr, guestMAC) || neighbor.State != netlink.NUD_PERMANENT {
				return errors.New("guest neighbor identity changed")
			}
		}
	}
	expectedNeighborCount := 0
	if expectRoutes {
		expectedNeighborCount = 1
	}
	if exactNeighbors != expectedNeighborCount {
		return fmt.Errorf("guest neighbor count changed: %d, want %d", exactNeighbors, expectedNeighborCount)
	}
	return nil
}

func isDefaultIPv4Route(destination *net.IPNet) bool {
	if destination == nil {
		return true
	}
	ones, bits := destination.Mask.Size()
	return bits == 32 && ones == 0
}

func verifyExactIPv4Address(handle *netlink.Handle, link netlink.Link, expected string) error {
	var (
		addresses []netlink.Addr
		err       error
	)
	if handle == nil {
		addresses, err = netlink.AddrList(link, netlink.FAMILY_V4)
	} else {
		addresses, err = handle.AddrList(link, netlink.FAMILY_V4)
	}
	if err != nil {
		return err
	}
	if len(addresses) != 1 || addresses[0].IPNet == nil || addresses[0].IPNet.String() != expected {
		return fmt.Errorf("expected only %s, got %v", expected, addresses)
	}
	return nil
}

func verifyRoutedNamespaceSysctls() error {
	for path, expected := range map[string]string{
		"/proc/sys/net/ipv4/ip_forward":             "1",
		"/proc/sys/net/ipv4/conf/all/rp_filter":     "0",
		"/proc/sys/net/ipv4/conf/default/rp_filter": "0",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read namespace sysctl %s: %w", path, err)
		}
		if strings.TrimSpace(string(raw)) != expected {
			return fmt.Errorf("namespace sysctl %s changed", path)
		}
	}
	return nil
}

func (binding *installedNetworkBinding) Failure() <-chan error {
	return binding.failure
}

func (binding *installedNetworkBinding) fence() error {
	rootVeth, err := netlink.LinkByName(binding.manifest.RootVethName)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			return nil
		}
		return err
	}
	if binding.rootIfindex <= 0 || rootVeth.Attrs().Index != binding.rootIfindex {
		return errors.New("refusing to fence replacement root veth")
	}
	return netlink.LinkSetDown(rootVeth)
}

func (binding *installedNetworkBinding) Deactivate() error {
	if binding == nil {
		return nil
	}
	binding.mu.Lock()
	defer binding.mu.Unlock()
	fenceErr := binding.fence()
	if binding.secretProxy != nil {
		fenceErr = errors.Join(fenceErr, binding.secretProxy.Close())
	}
	var packetErr error
	if binding.packet != nil {
		packetErr = withNetworkNamespace(binding.namespace, binding.packet.Deactivate)
	}
	if !binding.manifest.Installed {
		binding.active = false
		return errors.Join(fenceErr, packetErr)
	}
	var namespaceErr error
	if binding.namespace.IsOpen() {
		nsHandle, err := netlink.NewHandleAt(binding.namespace)
		if err != nil {
			namespaceErr = fmt.Errorf("open namespace for deactivation: %w", err)
		} else {
			defer nsHandle.Close()
			if tap, findErr := nsHandle.LinkByName(binding.manifest.TapName); findErr != nil {
				namespaceErr = errors.Join(namespaceErr, fmt.Errorf("find TAP for deactivation: %w", findErr))
			} else {
				namespaceErr = errors.Join(namespaceErr, nsHandle.LinkSetDown(tap))
			}
			if nsVeth, findErr := nsHandle.LinkByName(binding.manifest.NamespaceVethName); findErr != nil {
				namespaceErr = errors.Join(namespaceErr, fmt.Errorf("find namespace veth for deactivation: %w", findErr))
			} else {
				namespaceErr = errors.Join(namespaceErr, nsHandle.LinkSetDown(nsVeth))
			}
		}
	}
	binding.active = false
	verifyErr := binding.verifyLocked(false)
	return errors.Join(fenceErr, packetErr, namespaceErr, verifyErr)
}

func (binding *installedNetworkBinding) Close() error {
	if binding == nil {
		return nil
	}
	binding.stopOnce.Do(func() { close(binding.stop) })
	if binding.done != nil {
		<-binding.done
	}
	deactivateErr := binding.Deactivate()
	var packetErr error
	if binding.packet != nil && binding.namespace.IsOpen() {
		packetErr = withNetworkNamespace(binding.namespace, binding.packet.Close)
	}
	var namespaceErr error
	if binding.namespace.IsOpen() {
		namespaceErr = binding.namespace.Close()
	}
	return errors.Join(deactivateErr, packetErr, namespaceErr)
}
