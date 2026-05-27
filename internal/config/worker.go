package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

func LoadWorker() (Worker, error) {
	cfg := Worker{
		ControlURL:                   os.Getenv("HELMR_CONTROL_URL"),
		CASURI:                       os.Getenv("HELMR_CAS_URI"),
		WorkerBootstrapToken:         os.Getenv("HELMR_WORKER_BOOTSTRAP_TOKEN"),
		WorkerBootstrapTokenPath:     os.Getenv("HELMR_WORKER_BOOTSTRAP_TOKEN_PATH"),
		WorkerInstanceCredentialPath: os.Getenv("HELMR_WORKER_INSTANCE_CREDENTIAL_PATH"),
		CheckpointKey:                os.Getenv("HELMR_CHECKPOINT_ENCRYPTION_KEY"),
		WorkerResourceID:             env("HELMR_WORKER_RESOURCE_ID", hostname()),
		WorkerRegion:                 strings.TrimSpace(os.Getenv("HELMR_WORKER_REGION")),
		WorkDir:                      os.Getenv("HELMR_WORKER_WORK_DIR"),
		ImagesDir:                    os.Getenv("HELMR_WORKER_IMAGES_DIR"),
		GitPath:                      env("HELMR_GIT_PATH", "git"),
		BuildKitAddr:                 os.Getenv("HELMR_WORKER_BUILDKIT_ADDR"),
		BuildKitCacheNS:              env("HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE", "helmr"),
		FirecrackerPath:              env("HELMR_WORKER_FIRECRACKER_PATH", "firecracker"),
		JailerPath:                   env("HELMR_WORKER_FIRECRACKER_JAILER_PATH", "jailer"),
		JailerNumaNode:               0,
		JailerChrootDir:              os.Getenv("HELMR_WORKER_FIRECRACKER_CHROOT_DIR"),
		CgroupVersion:                env("HELMR_WORKER_FIRECRACKER_CGROUP_VERSION", "2"),
		CNINetworkName:               env("HELMR_WORKER_CNI_NETWORK", "helmr"),
		CNIProfile:                   os.Getenv("HELMR_WORKER_CNI_PROFILE"),
		CNIConfDir:                   env("HELMR_WORKER_CNI_CONF_DIR", "/etc/cni/conf.d"),
		CNIBinDir:                    env("HELMR_WORKER_CNI_BIN_DIR", "/opt/cni/bin"),
		CNICacheDir:                  os.Getenv("HELMR_WORKER_CNI_CACHE_DIR"),
		IPPath:                       env("HELMR_WORKER_IP_PATH", "ip"),
		NFTPath:                      env("HELMR_WORKER_NFT_PATH", "nft"),
		NetworkBlockedIPv4CIDRs:      envList("HELMR_WORKER_NETWORK_BLOCKED_IPV4_CIDRS"),
		NetworkBlockedIPv6CIDRs:      envList("HELMR_WORKER_NETWORK_BLOCKED_IPV6_CIDRS"),
		VMVCPUCount:                  2,
		VMMemoryMiB:                  2048,
		VMScratchDiskMiB:             8192,
		VMHealthTimeout:              30 * time.Second,
		PollEvery:                    2 * time.Second,
	}
	var err error
	if cfg.WorkerLabels, err = envLabels("HELMR_WORKER_LABELS"); err != nil {
		return cfg, err
	}
	if cfg.VMVCPUCount, err = envInt64("HELMR_VM_VCPUS", cfg.VMVCPUCount); err != nil {
		return cfg, err
	}
	if cfg.VMMemoryMiB, err = envInt64("HELMR_VM_MEMORY_MIB", cfg.VMMemoryMiB); err != nil {
		return cfg, err
	}
	if cfg.VMScratchDiskMiB, err = envInt64("HELMR_VM_SCRATCH_DISK_MIB", cfg.VMScratchDiskMiB); err != nil {
		return cfg, err
	}
	if cfg.VMScratchDiskMiB <= 0 {
		return cfg, errors.New("HELMR_VM_SCRATCH_DISK_MIB must be positive")
	}
	if cfg.WorkerDiskMiB, err = envInt64("HELMR_WORKER_DISK_MIB", cfg.WorkerDiskMiB); err != nil {
		return cfg, err
	}
	if cfg.WorkerDiskMiB < 0 {
		return cfg, errors.New("HELMR_WORKER_DISK_MIB must be non-negative")
	}
	if cfg.VMHealthTimeout, err = envDuration("HELMR_VM_HEALTH_TIMEOUT", cfg.VMHealthTimeout); err != nil {
		return cfg, err
	}
	if cfg.JailerUID, err = envInt("HELMR_WORKER_FIRECRACKER_JAILER_UID", cfg.JailerUID); err != nil {
		return cfg, err
	}
	if cfg.JailerGID, err = envInt("HELMR_WORKER_FIRECRACKER_JAILER_GID", cfg.JailerGID); err != nil {
		return cfg, err
	}
	if cfg.JailerNumaNode, err = envInt("HELMR_WORKER_FIRECRACKER_NUMA_NODE", cfg.JailerNumaNode); err != nil {
		return cfg, err
	}
	if cfg.CNIProfile == "" {
		cfg.CNIProfile = cfg.CNINetworkName + "/v0"
	}
	if cfg.ControlURL == "" {
		return cfg, errors.New("HELMR_CONTROL_URL is required")
	}
	if cfg.CASURI == "" {
		return cfg, errors.New("HELMR_CAS_URI is required")
	}
	if cfg.CheckpointKey == "" {
		return cfg, errors.New("HELMR_CHECKPOINT_ENCRYPTION_KEY is required")
	}
	if cfg.JailerUID <= 0 {
		return cfg, errors.New("HELMR_WORKER_FIRECRACKER_JAILER_UID is required")
	}
	if cfg.JailerGID <= 0 {
		return cfg, errors.New("HELMR_WORKER_FIRECRACKER_JAILER_GID is required")
	}
	return cfg, nil
}

func envLabels(name string) (map[string]string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return map[string]string{}, nil
	}
	labels := map[string]string{}
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, rawValue, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("%s label %q must be key=value", name, part)
		}
		key = strings.TrimSpace(key)
		rawValue = strings.TrimSpace(rawValue)
		if key == "" {
			return nil, fmt.Errorf("%s label key is required", name)
		}
		labels[key] = rawValue
	}
	return labels, nil
}

func LoadWorkerControl() (WorkerControl, error) {
	cfg := WorkerControl{
		ControlURL:                   os.Getenv("HELMR_CONTROL_URL"),
		WorkerInstanceCredentialPath: os.Getenv("HELMR_WORKER_INSTANCE_CREDENTIAL_PATH"),
		WorkDir:                      os.Getenv("HELMR_WORKER_WORK_DIR"),
		PollEvery:                    2 * time.Second,
	}
	if cfg.ControlURL == "" {
		return cfg, errors.New("HELMR_CONTROL_URL is required")
	}
	return cfg, nil
}
