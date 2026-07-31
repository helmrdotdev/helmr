//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/worker/datapath"
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

type networkOwnerManifest struct {
	Version             string `json:"version"`
	NetworkABI          string `json:"network_abi"`
	OwnerKind           string `json:"owner_kind"`
	OwnerID             string `json:"owner_id"`
	WorkerEpoch         int64  `json:"worker_epoch"`
	Generation          int64  `json:"generation"`
	AllocationIndex     uint32 `json:"allocation_index"`
	NamespaceName       string `json:"namespace_name"`
	RootVethName        string `json:"root_veth_name"`
	NamespaceVethName   string `json:"namespace_veth_name"`
	TapName             string `json:"tap_name"`
	RootIPv4CIDR        string `json:"root_ipv4_cidr"`
	NamespaceIPv4CIDR   string `json:"namespace_ipv4_cidr"`
	TranslationIPv4CIDR string `json:"translation_ipv4_cidr"`
	GuestIPv4CIDR       string `json:"guest_ipv4_cidr"`
	GuestMAC            string `json:"guest_mac"`
	GatewayIPv4         string `json:"gateway_ipv4"`
	GatewayMAC          string `json:"gateway_mac"`
	ResolverIPv4        string `json:"resolver_ipv4"`
	GuestInterfaceName  string `json:"guest_interface_name"`
	MTU                 int    `json:"mtu"`
	RootTableName       string `json:"root_table_name"`
	Installed           bool   `json:"installed"`
	RootIfindex         int    `json:"root_ifindex"`
	NamespaceIfindex    int    `json:"namespace_ifindex"`
	TapIfindex          int    `json:"tap_ifindex"`
	NamespacePolicyHash string `json:"namespace_policy_hash"`
	RootPolicyHash      string `json:"root_policy_hash"`
	BPFProgramID        int    `json:"bpf_program_id"`
	BPFProgramTag       string `json:"bpf_program_tag"`
	BPFFilterHandle     uint32 `json:"bpf_filter_handle"`
	PacketMark          uint32 `json:"packet_mark"`
}

type installedNetworkBinding struct {
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
	owner vm.Owner,
	logical vm.WorkloadBinding,
	installed **installedNetworkBinding,
) firecracker.Opt {
	return func(machine *firecracker.Machine) {
		machine.Handlers.FcInit = machine.Handlers.FcInit.Prepend(firecracker.Handler{
			Name: "helmr.InstallNetworkBinding",
			Fn: func(ctx context.Context, _ *firecracker.Machine) error {
				binding, err := c.prepareNetworkBinding(ctx, owner, logical)
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
	if err := c.installRoutedPolicy(ctx, binding, owner.Kind != vm.OwnerRuntime); err != nil {
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

func (c *Connector) allocateNetworkOwner(owner vm.Owner, logical vm.WorkloadBinding) (networkOwnerManifest, error) {
	if _, _, err := configuredNetworkPools(c.cfg); err != nil {
		return networkOwnerManifest{}, err
	}
	lockPath := filepath.Join(c.cfg.StateDir, ".network.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return networkOwnerManifest{}, fmt.Errorf("open network allocation lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return networkOwnerManifest{}, fmt.Errorf("lock network allocation: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	used := make(map[uint32]struct{})
	entries, err := os.ReadDir(c.cfg.StateDir)
	if err != nil {
		return networkOwnerManifest{}, fmt.Errorf("inventory network owners: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == owner.ID {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(c.cfg.StateDir, entry.Name(), networkManifestName))
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			return networkOwnerManifest{}, fmt.Errorf("read network owner %q: %w", entry.Name(), readErr)
		}
		var existing networkOwnerManifest
		if err := json.Unmarshal(raw, &existing); err != nil || existing.Version != networkManifestVersion {
			return networkOwnerManifest{}, fmt.Errorf("network owner %q is not valid v0 state", entry.Name())
		}
		existingOwner := vm.Owner{Kind: vm.OwnerKind(existing.OwnerKind), ID: existing.OwnerID}
		if err := c.validateNetworkOwnerManifest(existing, existingOwner); err != nil {
			return networkOwnerManifest{}, fmt.Errorf("network owner %q is not exact v0 state: %w", entry.Name(), err)
		}
		used[existing.AllocationIndex] = struct{}{}
	}
	var index uint32
	found := false
	for candidate := 0; candidate < c.cfg.NetworkCapacity; candidate++ {
		index = uint32(candidate)
		if _, exists := used[index]; !exists {
			found = true
			break
		}
	}
	if !found {
		return networkOwnerManifest{}, errors.New("worker network attachment capacity is exhausted")
	}
	manifest, err := c.networkOwnerManifest(owner, logical.WorkerEpoch, logical.Generation, index)
	if err != nil {
		return networkOwnerManifest{}, err
	}
	path := filepath.Join(c.cfg.StateDir, owner.ID, networkManifestName)
	if err := writeNetworkOwnerManifest(path, manifest, true); err != nil {
		return networkOwnerManifest{}, err
	}
	return manifest, nil
}

func (c *Connector) networkOwnerManifest(owner vm.Owner, workerEpoch, generation int64, allocationIndex uint32) (networkOwnerManifest, error) {
	linkPool, translationPool, err := configuredNetworkPools(c.cfg)
	if err != nil {
		return networkOwnerManifest{}, err
	}
	rootIPv4, err := prefixAddress(linkPool, uint64(allocationIndex)*2)
	if err != nil {
		return networkOwnerManifest{}, err
	}
	namespaceIPv4, err := prefixAddress(linkPool, uint64(allocationIndex)*2+1)
	if err != nil {
		return networkOwnerManifest{}, err
	}
	translationIPv4, err := prefixAddress(translationPool, uint64(allocationIndex))
	if err != nil {
		return networkOwnerManifest{}, err
	}
	digest := sha256.Sum256([]byte(owner.ID + "\x00" + strconv.FormatInt(generation, 10)))
	suffix := hex.EncodeToString(digest[:])
	return networkOwnerManifest{
		Version: networkManifestVersion, NetworkABI: NetworkABIV0,
		OwnerKind: string(owner.Kind), OwnerID: owner.ID,
		WorkerEpoch: workerEpoch, Generation: generation,
		AllocationIndex: allocationIndex, NamespaceName: owner.ID,
		RootVethName: "hr" + suffix[:11], NamespaceVethName: namespaceVethName,
		TapName: GuestTapNameV0, RootIPv4CIDR: rootIPv4.String() + "/31",
		NamespaceIPv4CIDR:   namespaceIPv4.String() + "/31",
		TranslationIPv4CIDR: translationIPv4.String() + "/32",
		GuestIPv4CIDR:       GuestNetworkCIDRV0, GuestMAC: GuestMACV0,
		GatewayIPv4: GuestGatewayIPv4V0, GatewayMAC: GuestGatewayMACV0,
		ResolverIPv4: c.cfg.NetworkResolverIPv4, GuestInterfaceName: GuestInterfaceNameV0,
		MTU: GuestMTUV0, RootTableName: "hmr_" + suffix[:12],
	}, nil
}

func (c *Connector) persistInstalledNetworkOwner(binding *installedNetworkBinding) error {
	identity, err := binding.packet.Identity()
	if err != nil {
		return fmt.Errorf("read installed ingress identity: %w", err)
	}
	manifest := binding.manifest
	manifest.Installed = true
	manifest.RootIfindex = binding.rootIfindex
	manifest.NamespaceIfindex = binding.namespaceIfindex
	manifest.TapIfindex = binding.tapIfindex
	manifest.NamespacePolicyHash = binding.namespacePolicyFingerprint
	manifest.RootPolicyHash = binding.rootPolicyFingerprint
	manifest.BPFProgramID = identity.ProgramID
	manifest.BPFProgramTag = identity.ProgramTag
	manifest.BPFFilterHandle = identity.FilterHandle
	manifest.PacketMark = identity.Mark
	if identity.Ifindex != manifest.TapIfindex {
		return errors.New("installed ingress identity does not match exact TAP")
	}
	path := filepath.Join(c.cfg.StateDir, manifest.OwnerID, networkManifestName)
	if err := writeNetworkOwnerManifest(path, manifest, false); err != nil {
		return err
	}
	binding.manifest = manifest
	return nil
}

func writeNetworkOwnerManifest(path string, manifest networkOwnerManifest, exclusive bool) error {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode network owner manifest: %w", err)
	}
	directory := filepath.Dir(path)
	if exclusive {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("create network owner manifest: %w", err)
		}
		if err := writeAndSync(file, append(raw, '\n')); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close network owner manifest: %w", err)
		}
		return syncDirectory(directory)
	}
	temporary, err := os.CreateTemp(directory, ".network-owner-")
	if err != nil {
		return fmt.Errorf("create network owner manifest replacement: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect network owner manifest replacement: %w", err)
	}
	if err := writeAndSync(temporary, append(raw, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close network owner manifest replacement: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit network owner manifest replacement: %w", err)
	}
	return syncDirectory(directory)
}

func writeAndSync(file *os.File, raw []byte) error {
	if _, err := file.Write(raw); err != nil {
		return fmt.Errorf("write network owner manifest: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync network owner manifest: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open network owner directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync network owner directory: %w", err)
	}
	return nil
}

func (c *Connector) validateNetworkOwnerManifest(manifest networkOwnerManifest, owner vm.Owner) error {
	if err := owner.Validate(); err != nil {
		return err
	}
	if manifest.Version != networkManifestVersion || manifest.NetworkABI != NetworkABIV0 {
		return errors.New("network owner manifest ABI is invalid")
	}
	if manifest.OwnerKind != string(owner.Kind) || manifest.OwnerID != owner.ID || manifest.NamespaceName != owner.ID {
		return errors.New("network owner manifest does not exact-match owner")
	}
	if manifest.WorkerEpoch <= 0 || manifest.Generation <= 0 {
		return errors.New("network owner manifest authority is incomplete")
	}
	if owner.Kind == vm.OwnerRuntime && manifest.Generation != 1 {
		return errors.New("runtime network owner generation must be one")
	}
	if int(manifest.AllocationIndex) >= c.cfg.NetworkCapacity {
		return errors.New("network owner allocation is outside configured capacity")
	}
	expected, err := c.networkOwnerManifest(owner, manifest.WorkerEpoch, manifest.Generation, manifest.AllocationIndex)
	if err != nil {
		return err
	}
	static := manifest
	static.Installed = false
	static.RootIfindex = 0
	static.NamespaceIfindex = 0
	static.TapIfindex = 0
	static.NamespacePolicyHash = ""
	static.RootPolicyHash = ""
	static.BPFProgramID = 0
	static.BPFProgramTag = ""
	static.BPFFilterHandle = 0
	static.PacketMark = 0
	if static != expected {
		return errors.New("network owner manifest physical identity is invalid")
	}
	if manifest.Installed {
		if manifest.RootIfindex <= 0 || manifest.NamespaceIfindex <= 0 || manifest.TapIfindex <= 0 ||
			manifest.NamespacePolicyHash == "" || manifest.RootPolicyHash == "" ||
			manifest.BPFProgramID <= 0 || manifest.BPFProgramTag == "" ||
			manifest.BPFFilterHandle == 0 || manifest.PacketMark == 0 {
			return errors.New("installed network owner manifest identity is incomplete")
		}
		return nil
	}
	if manifest.RootIfindex != 0 || manifest.NamespaceIfindex != 0 || manifest.TapIfindex != 0 ||
		manifest.NamespacePolicyHash != "" || manifest.RootPolicyHash != "" ||
		manifest.BPFProgramID != 0 || manifest.BPFProgramTag != "" ||
		manifest.BPFFilterHandle != 0 || manifest.PacketMark != 0 {
		return errors.New("inactive network owner manifest contains installed identity")
	}
	return nil
}

func configuredNetworkPools(cfg Config) (netip.Prefix, netip.Prefix, error) {
	linkPool, err := netip.ParsePrefix(strings.TrimSpace(cfg.NetworkLinkPool))
	if err != nil || !linkPool.Addr().Is4() || linkPool != linkPool.Masked() || linkPool.Bits() > 31 {
		return netip.Prefix{}, netip.Prefix{}, errors.New("worker network link pool must be canonical IPv4 CIDR with /31 capacity")
	}
	translationPool, err := netip.ParsePrefix(strings.TrimSpace(cfg.NetworkTranslationPool))
	if err != nil || !translationPool.Addr().Is4() || translationPool != translationPool.Masked() {
		return netip.Prefix{}, netip.Prefix{}, errors.New("worker network translation pool must be canonical IPv4 CIDR")
	}
	if prefixesOverlap(linkPool, translationPool) {
		return netip.Prefix{}, netip.Prefix{}, errors.New("worker network pools overlap")
	}
	guestPrefix := netip.MustParsePrefix(GuestNetworkCIDRV0).Masked()
	if prefixesOverlap(linkPool, guestPrefix) || prefixesOverlap(translationPool, guestPrefix) {
		return netip.Prefix{}, netip.Prefix{}, errors.New("worker network pool overlaps the v0 guest subnet")
	}
	if cfg.NetworkCapacity <= 0 || uint64(cfg.NetworkCapacity) > prefixCapacity(linkPool)/2 || uint64(cfg.NetworkCapacity) > prefixCapacity(translationPool) {
		return netip.Prefix{}, netip.Prefix{}, errors.New("worker network pools do not cover configured capacity")
	}
	resolver, err := netip.ParseAddr(strings.TrimSpace(cfg.NetworkResolverIPv4))
	if err != nil || !resolver.Is4() || resolver.IsUnspecified() {
		return netip.Prefix{}, netip.Prefix{}, errors.New("worker network resolver must be specified IPv4")
	}
	return linkPool, translationPool, nil
}

func validateNetworkPools(cfg Config) (netip.Prefix, netip.Prefix, error) {
	linkPool, translationPool, err := configuredNetworkPools(cfg)
	if err != nil {
		return netip.Prefix{}, netip.Prefix{}, err
	}
	if err := rejectLiveNetworkOverlap(linkPool, translationPool); err != nil {
		return netip.Prefix{}, netip.Prefix{}, err
	}
	return linkPool, translationPool, nil
}

func rejectLiveNetworkOverlap(pools ...netip.Prefix) error {
	addresses, err := netlink.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("inventory worker IPv4 addresses: %w", err)
	}
	for _, address := range addresses {
		if address.IP == nil {
			continue
		}
		parsed, ok := netip.AddrFromSlice(address.IP)
		if !ok || !parsed.Unmap().Is4() {
			continue
		}
		for _, pool := range pools {
			if pool.Contains(parsed.Unmap()) {
				return fmt.Errorf("worker network pool %s overlaps live address %s", pool, parsed.Unmap())
			}
		}
	}
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("inventory worker IPv4 routes: %w", err)
	}
	for _, route := range routes {
		if route.Dst == nil {
			continue
		}
		routePrefix, ok := netipPrefix(route.Dst)
		if !ok {
			continue
		}
		for _, pool := range pools {
			if networkPoolRouteConflict(pool, routePrefix) {
				return fmt.Errorf("worker network pool %s overlaps live route %s", pool, routePrefix)
			}
		}
	}
	return nil
}

func networkPoolRouteConflict(pool, routePrefix netip.Prefix) bool {
	return routePrefix.Bits() > 0 && prefixesOverlap(pool, routePrefix)
}

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

func (c *Connector) installRoutedPolicy(ctx context.Context, binding *installedNetworkBinding, build bool) error {
	m := binding.manifest
	guestIP := strings.Split(GuestNetworkCIDRV0, "/")[0]
	hostIPv4, err := hostIPv4Prefixes(netip.MustParsePrefix(GuestNetworkCIDRV0).Masked())
	if err != nil {
		return err
	}
	linkPool, _, err := configuredNetworkPools(c.cfg)
	if err != nil {
		return err
	}
	blocked := append(hostIPv4, linkPool)
	blocked = append(blocked, c.cfg.NetworkBlockedIPv4CIDRs...)
	var packet *datapath.Binding
	if err := withNetworkNamespace(binding.namespace, func() error {
		var prepareErr error
		guestIPv4 := netip.MustParsePrefix(GuestNetworkCIDRV0).Addr()
		gatewayIPv4 := netip.MustParseAddr(GuestGatewayIPv4V0)
		guestMAC, _ := net.ParseMAC(GuestMACV0)
		packet, prepareErr = c.datapath.Prepare(datapath.Authority{
			WorkerEpoch: binding.manifest.WorkerEpoch,
			OwnerID:     binding.manifest.OwnerID,
			Generation:  binding.manifest.Generation,
		}, datapath.InterfaceFacts{TapName: m.TapName, GuestIPv4: guestIPv4, GatewayIPv4: gatewayIPv4, GuestMAC: guestMAC})
		return prepareErr
	}); err != nil {
		return fmt.Errorf("prepare TAP ingress binding: %w", err)
	}
	binding.packet = packet
	if err != nil {
		return err
	}
	script, err := renderNetworkPolicy(networkPolicyInput{
		Tap: m.TapName, Peer: m.NamespaceVethName, Mark: packet.Mark(),
		BlockedIPv4CIDRs: blocked,
		ResolverIPv4:     c.cfg.NetworkResolverIPv4, Build: build,
		GuestIPv4: guestIP, TranslationIPv4: netip.MustParsePrefix(m.TranslationIPv4CIDR).Addr().String(),
	})
	if err != nil {
		return err
	}
	if err := c.applyNftScript(ctx, m.NamespaceName, script); err != nil {
		return err
	}
	rootScript := renderRootNetworkPolicy(m)
	if err := c.applyNftScript(ctx, "", rootScript); err != nil {
		return err
	}
	binding.namespacePolicyFingerprint, err = c.nftTableFingerprint(ctx, m.NamespaceName, networkPolicyTableName)
	if err != nil {
		return err
	}
	binding.rootPolicyFingerprint, err = c.nftTableFingerprint(ctx, "", m.RootTableName)
	if err != nil {
		return err
	}
	return nil
}

func installRoutedRoutes(binding *installedNetworkBinding) error {
	m := binding.manifest
	nsHandle, err := netlink.NewHandleAt(binding.namespace)
	if err != nil {
		return fmt.Errorf("open namespace for route preparation: %w", err)
	}
	defer nsHandle.Close()
	nsVeth, err := nsHandle.LinkByName(m.NamespaceVethName)
	if err != nil {
		return fmt.Errorf("find namespace veth for routes: %w", err)
	}
	rootVeth, err := netlink.LinkByName(m.RootVethName)
	if err != nil {
		return fmt.Errorf("find root veth for routes: %w", err)
	}
	rootIP := net.ParseIP(netip.MustParsePrefix(m.RootIPv4CIDR).Addr().String()).To4()
	if err := nsHandle.RouteAdd(&netlink.Route{LinkIndex: nsVeth.Attrs().Index, Gw: rootIP, Flags: int(unix.RTNH_F_ONLINK)}); err != nil {
		return fmt.Errorf("install namespace default route: %w", err)
	}
	nsIP := net.ParseIP(netip.MustParsePrefix(m.NamespaceIPv4CIDR).Addr().String()).To4()
	translationIP := net.ParseIP(netip.MustParsePrefix(m.TranslationIPv4CIDR).Addr().String()).To4()
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: rootVeth.Attrs().Index, Gw: nsIP, Dst: &net.IPNet{IP: translationIP, Mask: net.CIDRMask(32, 32)}, Flags: int(unix.RTNH_F_ONLINK)}); err != nil {
		return fmt.Errorf("install translation route: %w", err)
	}
	return nil
}

func renderRootNetworkPolicy(m networkOwnerManifest) string {
	table := m.RootTableName
	rootVeth := strconv.Quote(m.RootVethName)
	var script strings.Builder
	fmt.Fprintf(&script, "add table inet %s\n", table)
	fmt.Fprintf(&script, "add chain inet %s input { type filter hook input priority -10; policy accept; }\n", table)
	fmt.Fprintf(&script, "add chain inet %s output { type filter hook output priority -10; policy accept; }\n", table)
	fmt.Fprintf(&script, "add chain inet %s forward { type filter hook forward priority -10; policy accept; }\n", table)
	fmt.Fprintf(&script, "add chain inet %s postrouting { type nat hook postrouting priority srcnat; policy accept; }\n", table)
	fmt.Fprintf(&script, "add rule inet %s input iifname %s drop\n", table, rootVeth)
	fmt.Fprintf(&script, "add rule inet %s output oifname %s drop\n", table, rootVeth)
	fmt.Fprintf(&script, "add rule inet %s forward iifname %s oifname \"hr*\" drop\n", table, rootVeth)
	fmt.Fprintf(&script, "add rule inet %s forward iifname %s accept\n", table, rootVeth)
	fmt.Fprintf(&script, "add rule inet %s forward oifname %s ct state established,related accept\n", table, rootVeth)
	fmt.Fprintf(&script, "add rule inet %s forward oifname %s drop\n", table, rootVeth)
	fmt.Fprintf(&script, "add rule inet %s postrouting ip saddr %s oifname != %s masquerade\n", table, netip.MustParsePrefix(m.TranslationIPv4CIDR).Addr(), rootVeth)
	return script.String()
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
		if err := exec.CommandContext(ctx, c.cfg.IPPath, "netns", "delete", manifest.NamespaceName).Run(); err != nil {
			return fmt.Errorf("delete routed network namespace: %w", err)
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

func (c *Connector) applyNftScript(ctx context.Context, namespace string, script string) error {
	args := []string{"-f", "-"}
	command := c.cfg.NFTPath
	if namespace != "" {
		command = c.cfg.IPPath
		args = append([]string{"netns", "exec", namespace, c.cfg.NFTPath}, args...)
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("apply network policy: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (c *Connector) deleteNftTable(ctx context.Context, namespace string, table string) error {
	args := []string{"delete", "table", "inet", table}
	command := c.cfg.NFTPath
	if namespace != "" {
		command = c.cfg.IPPath
		args = append([]string{"netns", "exec", namespace, c.cfg.NFTPath}, args...)
	}
	cmd := exec.CommandContext(ctx, command, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.ToLower(stderr.String())
		if strings.Contains(detail, "no such file") || strings.Contains(detail, "does not exist") || strings.Contains(detail, "no such process") {
			return nil
		}
		return fmt.Errorf("delete network policy table %s: %w: %s", table, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (c *Connector) nftTableFingerprint(ctx context.Context, namespace string, table string) (string, error) {
	args := []string{"-j", "list", "table", "inet", table}
	command := c.cfg.NFTPath
	if namespace != "" {
		command = c.cfg.IPPath
		args = append([]string{"netns", "exec", namespace, c.cfg.NFTPath}, args...)
	}
	cmd := exec.CommandContext(ctx, command, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		detail := strings.ToLower(stderr.String())
		if strings.Contains(detail, "no such file") || strings.Contains(detail, "does not exist") || strings.Contains(detail, "no such process") {
			return "", fmt.Errorf("read network policy table %s: %w", table, os.ErrNotExist)
		}
		return "", fmt.Errorf("read network policy table %s: %w: %s", table, err, strings.TrimSpace(stderr.String()))
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return "", fmt.Errorf("decode network policy table %s: %w", table, err)
	}
	normalized := normalizeNftDocument(document, "")
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func normalizeNftDocument(value any, parent string) any {
	switch typed := value.(type) {
	case []any:
		for index := range typed {
			typed[index] = normalizeNftDocument(typed[index], parent)
		}
		return typed
	case map[string]any:
		delete(typed, "handle")
		if parent == "counter" {
			delete(typed, "packets")
			delete(typed, "bytes")
		}
		if parent == "quota" {
			delete(typed, "used")
		}
		for key, child := range typed {
			typed[key] = normalizeNftDocument(child, key)
		}
		return typed
	default:
		return value
	}
}

func (c *Connector) readNetworkCounters(ctx context.Context, netnsName string, label string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.cfg.IPPath, "netns", "exec", netnsName, c.cfg.NFTPath, "-j", "list", "counters", "table", "inet", networkPolicyTableName)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read %s network counters: %w: %s", label, err, strings.TrimSpace(stderr.String()))
	}
	return raw, nil
}

func (c *Connector) readRunNetworkStatus(ctx context.Context, netnsName string) (vm.RunNetworkStatus, error) {
	raw, err := c.readNetworkCounters(ctx, netnsName, "Run")
	if err != nil {
		return vm.RunNetworkStatus{}, err
	}
	return parseRunNetworkStatus(raw)
}

func (c *Connector) readBuildNetworkStatus(ctx context.Context, netnsName string) (vm.BuildNetworkStatus, error) {
	raw, err := c.readNetworkCounters(ctx, netnsName, "build")
	if err != nil {
		return vm.BuildNetworkStatus{}, err
	}
	return parseBuildNetworkStatus(raw)
}

func parseBuildNetworkStatus(raw []byte) (vm.BuildNetworkStatus, error) {
	counters, err := parseNetworkCounters(raw, "build")
	if err != nil {
		return vm.BuildNetworkStatus{}, err
	}
	denied, foundDenied := counters[buildNetworkDeniedCounterName]
	limit, foundLimit := counters[buildNetworkLimitCounterName]
	if !foundDenied || !foundLimit {
		return vm.BuildNetworkStatus{}, errors.New("build network counters are incomplete")
	}
	return vm.BuildNetworkStatus{DeniedPackets: denied, LimitPackets: limit}, nil
}

func hostIPv4Prefixes(excluded ...netip.Prefix) ([]netip.Prefix, error) {
	addresses, err := netlink.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("inventory worker IPv4 addresses: %w", err)
	}
	prefixes := make([]netip.Prefix, 0, len(addresses))
	for _, address := range addresses {
		parsed, ok := netip.AddrFromSlice(address.IP)
		if !ok || !parsed.Unmap().Is4() || parsed.IsUnspecified() {
			continue
		}
		parsed = parsed.Unmap()
		if slices.ContainsFunc(excluded, func(prefix netip.Prefix) bool { return prefix.Contains(parsed) }) {
			continue
		}
		prefixes = append(prefixes, netip.PrefixFrom(parsed, 32))
	}
	if len(prefixes) == 0 {
		return nil, errors.New("worker has no concrete IPv4 address")
	}
	return prefixes, nil
}

func withNetworkNamespace(target netns.NsHandle, fn func() error) (returnErr error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	current, err := netns.Get()
	if err != nil {
		return fmt.Errorf("open current network namespace: %w", err)
	}
	defer current.Close()
	if err := netns.Set(target); err != nil {
		return fmt.Errorf("enter network namespace: %w", err)
	}
	defer func() {
		if err := netns.Set(current); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("restore network namespace: %w", err))
		}
	}()
	return fn()
}

func prefixCapacity(prefix netip.Prefix) uint64 {
	return uint64(1) << uint(32-prefix.Bits())
}

func prefixAddress(prefix netip.Prefix, offset uint64) (netip.Addr, error) {
	if offset >= prefixCapacity(prefix) {
		return netip.Addr{}, fmt.Errorf("address offset %d exceeds pool %s", offset, prefix)
	}
	base := binary.BigEndian.Uint32(prefix.Addr().AsSlice())
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], base+uint32(offset))
	return netip.AddrFrom4(raw), nil
}

func prefixesOverlap(left, right netip.Prefix) bool {
	return left.Contains(right.Addr()) || right.Contains(left.Addr())
}

func netipPrefix(network *net.IPNet) (netip.Prefix, bool) {
	if network == nil {
		return netip.Prefix{}, false
	}
	address, ok := netip.AddrFromSlice(network.IP)
	ones, bits := network.Mask.Size()
	if !ok || !address.Unmap().Is4() || bits != 32 || ones < 0 {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(address.Unmap(), ones).Masked(), true
}

func staticNetworkInterface(resolverIPv4 string) firecracker.NetworkInterface {
	guestIP, guestNetwork, _ := net.ParseCIDR(GuestNetworkCIDRV0)
	guestNetwork.IP = guestIP
	return firecracker.NetworkInterface{StaticConfiguration: &firecracker.StaticNetworkConfiguration{
		HostDevName: GuestTapNameV0,
		MacAddress:  GuestMACV0,
		IPConfiguration: &firecracker.IPConfiguration{
			IPAddr: *guestNetwork, Gateway: net.ParseIP(GuestGatewayIPv4V0).To4(),
			Nameservers: []string{resolverIPv4}, IfName: GuestInterfaceNameV0,
		},
	}}
}
