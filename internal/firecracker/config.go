package firecracker

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultGuestPort = uint32(5000)
	HealthPort       = uint32(5001)

	DefaultFirecrackerPath = "firecracker"
	DefaultJailerPath      = "jailer"
	DefaultCNINetworkName  = "helmr"
	DefaultCNIConfDir      = "/etc/cni/conf.d"
	DefaultCNIBinDir       = "/opt/cni/bin"
	DefaultCNIIfName       = "veth0"
	DefaultCNIVMIfName     = "eth0"
	DefaultIPPath          = "ip"
	DefaultNFTPath         = "nft"
	DefaultKVMPath         = "/dev/kvm"
	DefaultVCPUs           = int64(2)
	DefaultMemoryMiB       = int64(2048)
	DefaultScratchDiskMiB  = int64(8192)
	DefaultCgroupVersion   = "2"
)

type Config struct {
	FirecrackerPath         string
	JailerPath              string
	JailerUID               int
	JailerGID               int
	JailerNumaNode          int
	JailerChrootBaseDir     string
	CgroupVersion           string
	KernelPath              string
	InitramfsPath           string
	RootfsPath              string
	StateDir                string
	CNINetworkName          string
	CNIProfile              string
	CNIConfDir              string
	CNIBinDir               string
	CNICacheDir             string
	CNIIfName               string
	CNIVMIfName             string
	IPPath                  string
	NFTPath                 string
	MkfsExt4Path            string
	NetworkBlockedIPv4CIDRs []string
	NetworkBlockedIPv6CIDRs []string
	KVMPath                 string
	VCPUCount               int64
	MemoryMiB               int64
	ScratchDiskMiB          int64
	GuestPort               uint32
	HealthPort              uint32
	HealthTimeout           time.Duration
}

type RuntimeCapabilities struct {
	Arch         string
	ABI          string
	KernelDigest string
	RootfsDigest string
	CNIProfile   string
	VCPUCount    int64
	MemoryMiB    int64
}

func (cfg Config) WithDefaults() Config {
	if strings.TrimSpace(cfg.FirecrackerPath) == "" {
		cfg.FirecrackerPath = DefaultFirecrackerPath
	}
	if strings.TrimSpace(cfg.JailerPath) == "" {
		cfg.JailerPath = DefaultJailerPath
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		cfg.StateDir = filepath.Join(os.TempDir(), "helmr-worker", "vms", "guest")
	}
	if strings.TrimSpace(cfg.JailerChrootBaseDir) == "" {
		cfg.JailerChrootBaseDir = filepath.Join(cfg.StateDir, "jailer")
	}
	if strings.TrimSpace(cfg.CgroupVersion) == "" {
		cfg.CgroupVersion = DefaultCgroupVersion
	}
	if strings.TrimSpace(cfg.CNINetworkName) == "" {
		cfg.CNINetworkName = DefaultCNINetworkName
	}
	if strings.TrimSpace(cfg.CNIProfile) == "" {
		cfg.CNIProfile = cfg.CNINetworkName + "/v1"
	}
	if strings.TrimSpace(cfg.CNIConfDir) == "" {
		cfg.CNIConfDir = DefaultCNIConfDir
	}
	if strings.TrimSpace(cfg.CNIBinDir) == "" {
		cfg.CNIBinDir = DefaultCNIBinDir
	}
	if strings.TrimSpace(cfg.CNIIfName) == "" {
		cfg.CNIIfName = DefaultCNIIfName
	}
	if strings.TrimSpace(cfg.CNIVMIfName) == "" {
		cfg.CNIVMIfName = DefaultCNIVMIfName
	}
	if strings.TrimSpace(cfg.IPPath) == "" {
		cfg.IPPath = DefaultIPPath
	}
	if strings.TrimSpace(cfg.NFTPath) == "" {
		cfg.NFTPath = DefaultNFTPath
	}
	if strings.TrimSpace(cfg.MkfsExt4Path) == "" {
		cfg.MkfsExt4Path = "mkfs.ext4"
	}
	if strings.TrimSpace(cfg.KVMPath) == "" {
		cfg.KVMPath = DefaultKVMPath
	}
	if cfg.VCPUCount == 0 {
		cfg.VCPUCount = DefaultVCPUs
	}
	if cfg.MemoryMiB == 0 {
		cfg.MemoryMiB = DefaultMemoryMiB
	}
	if cfg.ScratchDiskMiB == 0 {
		cfg.ScratchDiskMiB = DefaultScratchDiskMiB
	}
	if cfg.GuestPort == 0 {
		cfg.GuestPort = DefaultGuestPort
	}
	if cfg.HealthPort == 0 {
		cfg.HealthPort = HealthPort
	}
	if cfg.HealthTimeout == 0 {
		cfg.HealthTimeout = 30 * time.Second
	}
	return cfg
}

func (cfg Config) Validate() error {
	var problems []error
	if strings.TrimSpace(cfg.FirecrackerPath) == "" {
		problems = append(problems, errors.New("firecracker path is required"))
	}
	if strings.TrimSpace(cfg.JailerPath) == "" {
		problems = append(problems, errors.New("firecracker jailer path is required"))
	}
	if cfg.JailerUID <= 0 {
		problems = append(problems, fmt.Errorf("firecracker jailer uid must be positive, got %d", cfg.JailerUID))
	}
	if cfg.JailerGID <= 0 {
		problems = append(problems, fmt.Errorf("firecracker jailer gid must be positive, got %d", cfg.JailerGID))
	}
	if cfg.JailerNumaNode < 0 {
		problems = append(problems, fmt.Errorf("firecracker jailer numa node must be non-negative, got %d", cfg.JailerNumaNode))
	}
	if strings.TrimSpace(cfg.JailerChrootBaseDir) == "" {
		problems = append(problems, errors.New("firecracker jailer chroot base directory is required"))
	}
	if strings.TrimSpace(cfg.CgroupVersion) == "" {
		problems = append(problems, errors.New("firecracker cgroup version is required"))
	}
	if strings.TrimSpace(cfg.KernelPath) == "" {
		problems = append(problems, errors.New("guest kernel path is required"))
	}
	if strings.TrimSpace(cfg.InitramfsPath) == "" {
		problems = append(problems, errors.New("guest initramfs path is required"))
	}
	if strings.TrimSpace(cfg.RootfsPath) == "" {
		problems = append(problems, errors.New("guest rootfs path is required"))
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		problems = append(problems, errors.New("firecracker state dir is required"))
	}
	if strings.TrimSpace(cfg.CNINetworkName) == "" {
		problems = append(problems, errors.New("guest CNI network name is required"))
	}
	if strings.TrimSpace(cfg.CNIProfile) == "" {
		problems = append(problems, errors.New("guest CNI profile is required"))
	}
	if strings.TrimSpace(cfg.CNIConfDir) == "" {
		problems = append(problems, errors.New("guest CNI config directory is required"))
	}
	if strings.TrimSpace(cfg.CNIBinDir) == "" {
		problems = append(problems, errors.New("guest CNI plugin directory is required"))
	}
	if strings.TrimSpace(cfg.CNIIfName) == "" {
		problems = append(problems, errors.New("guest CNI interface name is required"))
	}
	if strings.TrimSpace(cfg.CNIVMIfName) == "" {
		problems = append(problems, errors.New("guest VM network interface name is required"))
	}
	if cfg.VCPUCount <= 0 {
		problems = append(problems, fmt.Errorf("guest vcpu count must be positive, got %d", cfg.VCPUCount))
	}
	if cfg.MemoryMiB <= 0 {
		problems = append(problems, fmt.Errorf("guest memory must be positive, got %d MiB", cfg.MemoryMiB))
	}
	if cfg.ScratchDiskMiB <= 0 {
		problems = append(problems, fmt.Errorf("guest scratch disk must be positive, got %d MiB", cfg.ScratchDiskMiB))
	}
	if strings.TrimSpace(cfg.MkfsExt4Path) == "" {
		problems = append(problems, errors.New("mkfs.ext4 path is required"))
	}
	if strings.TrimSpace(cfg.KVMPath) == "" {
		problems = append(problems, errors.New("firecracker KVM path is required"))
	}
	if cfg.NetworkBlockedIPv4CIDRs == nil {
		problems = append(problems, errors.New("firecracker network blocked IPv4 CIDRs are required"))
	}
	if cfg.NetworkBlockedIPv6CIDRs == nil {
		problems = append(problems, errors.New("firecracker network blocked IPv6 CIDRs are required"))
	}
	if err := validateNetworkPolicyCIDRs("firecracker network blocked IPv4 CIDR", cfg.NetworkBlockedIPv4CIDRs, true); err != nil {
		problems = append(problems, err)
	}
	if err := validateNetworkPolicyCIDRs("firecracker network blocked IPv6 CIDR", cfg.NetworkBlockedIPv6CIDRs, false); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

func validateNetworkPolicyCIDRs(name string, cidrs []string, ipv4 bool) error {
	var problems []error
	for _, cidr := range cidrs {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			problems = append(problems, fmt.Errorf("%s must not be empty", name))
			continue
		}
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s %q must be a CIDR prefix: %w", name, cidr, err))
			continue
		}
		if ipv4 && !prefix.Addr().Is4() {
			problems = append(problems, fmt.Errorf("%s %q must be IPv4", name, cidr))
		}
		if !ipv4 && !prefix.Addr().Is6() {
			problems = append(problems, fmt.Errorf("%s %q must be IPv6", name, cidr))
		}
	}
	return errors.Join(problems...)
}
