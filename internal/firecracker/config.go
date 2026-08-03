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

	DefaultFirecrackerPath      = "firecracker"
	DefaultJailerPath           = "jailer"
	DefaultIPPath               = "ip"
	DefaultNFTPath              = "nft"
	DefaultKVMPath              = "/dev/kvm"
	DefaultVCPUs                = int64(2)
	DefaultMemoryMiB            = int64(2048)
	DefaultScratchDiskMiB       = int64(8192)
	DefaultCgroupVersion        = "2"
	DefaultInitTimeout          = 30 * time.Second
	DefaultHealthTimeout        = 30 * time.Second
	DefaultHealthAttemptTimeout = 5 * time.Second
	runtimeABI                  = "helmr.firecracker.snapshot.v0"
	NetworkABIV0                = "helmr/v0"
	GuestNetworkCIDRV0          = "192.168.127.2/30"
	GuestGatewayIPv4V0          = "192.168.127.1"
	GuestGatewayMACV0           = "02:fc:00:00:00:01"
	GuestMACV0                  = "02:fc:00:00:00:02"
	GuestTapNameV0              = "tap0"
	GuestInterfaceNameV0        = "eth0"
	GuestMTUV0                  = 1500
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
	RuntimeArtifactsPath    string
	StateDir                string
	NetworkLinkPool         string
	NetworkTranslationPool  string
	NetworkResolverIPv4     string
	NetworkBlockedIPv4CIDRs []netip.Prefix
	NetworkCapacity         int
	IPPath                  string
	NFTPath                 string
	MkfsExt4Path            string
	KVMPath                 string
	VCPUCount               int64
	MemoryMiB               int64
	ScratchDiskMiB          int64
	GuestPort               uint32
	HealthPort              uint32
	InitTimeout             time.Duration
	HealthTimeout           time.Duration
	HealthAttemptTimeout    time.Duration
}

type RuntimeCapabilities struct {
	ID              string
	Arch            string
	ABI             string
	KernelDigest    string
	InitramfsDigest string
	RootfsDigest    string
	NetworkABI      string
	VCPUCount       int64
	MemoryMiB       int64
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
		cfg.JailerChrootBaseDir = filepath.Join(filepath.Dir(filepath.Clean(cfg.StateDir)), "jailer")
	}
	if strings.TrimSpace(cfg.CgroupVersion) == "" {
		cfg.CgroupVersion = DefaultCgroupVersion
	}
	if strings.TrimSpace(cfg.RuntimeArtifactsPath) == "" && strings.TrimSpace(cfg.RootfsPath) != "" {
		cfg.RuntimeArtifactsPath = filepath.Join(filepath.Dir(cfg.RootfsPath), "runtime-artifacts.json")
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
	if cfg.InitTimeout == 0 {
		cfg.InitTimeout = DefaultInitTimeout
	}
	if cfg.HealthTimeout == 0 {
		cfg.HealthTimeout = DefaultHealthTimeout
	}
	if cfg.HealthAttemptTimeout == 0 {
		cfg.HealthAttemptTimeout = DefaultHealthAttemptTimeout
		if cfg.HealthTimeout > 0 && cfg.HealthAttemptTimeout > cfg.HealthTimeout {
			cfg.HealthAttemptTimeout = cfg.HealthTimeout
		}
	}
	return cfg
}

func (cfg Config) Validate() error {
	var problems []error
	if strings.TrimSpace(cfg.FirecrackerPath) == "" {
		problems = append(problems, errors.New("the Firecracker path is required"))
	}
	if strings.TrimSpace(cfg.JailerPath) == "" {
		problems = append(problems, errors.New("the Firecracker jailer path is required"))
	}
	if cfg.JailerUID <= 0 {
		problems = append(problems, fmt.Errorf("the Firecracker jailer uid must be positive, got %d", cfg.JailerUID))
	}
	if cfg.JailerGID <= 0 {
		problems = append(problems, fmt.Errorf("the Firecracker jailer gid must be positive, got %d", cfg.JailerGID))
	}
	if cfg.JailerNumaNode < 0 {
		problems = append(problems, fmt.Errorf("the Firecracker jailer NUMA node must be non-negative, got %d", cfg.JailerNumaNode))
	}
	if strings.TrimSpace(cfg.JailerChrootBaseDir) == "" {
		problems = append(problems, errors.New("the Firecracker jailer chroot base directory is required"))
	}
	if strings.TrimSpace(cfg.CgroupVersion) == "" {
		problems = append(problems, errors.New("the Firecracker cgroup version is required"))
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
	if strings.TrimSpace(cfg.RuntimeArtifactsPath) == "" {
		problems = append(problems, errors.New("guest runtime artifacts manifest path is required"))
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		problems = append(problems, errors.New("the Firecracker state dir is required"))
	}
	if pathsOverlap(cfg.StateDir, cfg.JailerChrootBaseDir) {
		problems = append(problems, errors.New("the Firecracker state dir and jailer chroot base directory must be disjoint"))
	}
	if strings.TrimSpace(cfg.NetworkLinkPool) == "" {
		problems = append(problems, errors.New("worker network link pool is required"))
	}
	if strings.TrimSpace(cfg.NetworkTranslationPool) == "" {
		problems = append(problems, errors.New("worker network translation pool is required"))
	}
	if strings.TrimSpace(cfg.NetworkResolverIPv4) == "" {
		problems = append(problems, errors.New("worker network resolver IPv4 is required"))
	}
	if cfg.NetworkCapacity <= 0 {
		problems = append(problems, errors.New("worker network capacity must be positive"))
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
		problems = append(problems, errors.New("the Firecracker KVM path is required"))
	}
	if cfg.InitTimeout <= 0 {
		problems = append(problems, fmt.Errorf("VMM API initialization timeout must be positive, got %s", cfg.InitTimeout))
	}
	if cfg.HealthTimeout <= 0 {
		problems = append(problems, fmt.Errorf("guest health timeout must be positive, got %s", cfg.HealthTimeout))
	}
	if cfg.HealthAttemptTimeout <= 0 {
		problems = append(problems, fmt.Errorf("guest health attempt timeout must be positive, got %s", cfg.HealthAttemptTimeout))
	}
	if cfg.HealthAttemptTimeout > cfg.HealthTimeout {
		problems = append(problems, fmt.Errorf("guest health attempt timeout %s must be less than or equal to guest health timeout %s", cfg.HealthAttemptTimeout, cfg.HealthTimeout))
	}
	return errors.Join(problems...)
}

func pathsOverlap(left string, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	if absolute, err := filepath.Abs(left); err == nil {
		left = absolute
	}
	if absolute, err := filepath.Abs(right); err == nil {
		right = absolute
	}
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	return pathContains(left, right) || pathContains(right, left)
}

func pathContains(parent string, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}
