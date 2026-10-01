//go:build linux

package firecracker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sys/unix"
)

type networkOwnerManifest struct {
	Version             string `json:"version"`
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

func (c *Connector) allocateNetworkOwner(owner vm.Owner, logical vm.WorkloadBinding) (networkOwnerManifest, error) {
	if _, _, err := configuredNetworkPools(c.cfg); err != nil {
		return networkOwnerManifest{}, err
	}
	if err := checkSecureDirectory("the Firecracker coordination directory", stateCoordinationDir(c.cfg.StateDir)); err != nil {
		return networkOwnerManifest{}, err
	}
	lockPath := networkAllocationLockPath(c.cfg.StateDir)
	lockFD, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return networkOwnerManifest{}, fmt.Errorf("open network allocation lock: %w", err)
	}
	lock := os.NewFile(uintptr(lockFD), lockPath)
	defer lock.Close()
	var lockStat unix.Stat_t
	if err := unix.Fstat(lockFD, &lockStat); err != nil {
		return networkOwnerManifest{}, fmt.Errorf("inspect network allocation lock: %w", err)
	}
	if lockStat.Mode&unix.S_IFMT != unix.S_IFREG || lockStat.Uid != uint32(os.Geteuid()) {
		return networkOwnerManifest{}, errors.New("network allocation lock must be a regular file owned by the worker user")
	}
	if err := unix.Fchmod(lockFD, 0o600); err != nil {
		return networkOwnerManifest{}, fmt.Errorf("secure network allocation lock: %w", err)
	}
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
		Version:   networkManifestVersion,
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
	if manifest.Version != networkManifestVersion {
		return errors.New("network owner manifest version is invalid")
	}
	if manifest.OwnerKind != string(owner.Kind) || manifest.OwnerID != owner.ID || manifest.NamespaceName != owner.ID {
		return errors.New("network owner manifest does not exact-match owner")
	}
	if manifest.WorkerEpoch <= 0 || manifest.Generation <= 0 {
		return errors.New("network owner manifest authority is incomplete")
	}
	if owner.Kind == vm.OwnerInstance && manifest.Generation != 1 {
		return errors.New("instance network owner generation must be one")
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
