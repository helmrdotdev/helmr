package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

func LoadWorker() (Worker, error) {
	cfg := Worker{
		ControlURL:                   envString("HELMR_CONTROL_URL"),
		CASURI:                       envString("HELMR_CAS_URI"),
		WorkerBootstrapToken:         envString("HELMR_WORKER_BOOTSTRAP_TOKEN"),
		WorkerBootstrapTokenPath:     envString("HELMR_WORKER_BOOTSTRAP_TOKEN_PATH"),
		WorkerInstanceCredentialPath: envString("HELMR_WORKER_INSTANCE_CREDENTIAL_PATH"),
		CheckpointKey:                envString("HELMR_CHECKPOINT_ENCRYPTION_KEY"),
		WorkerResourceID:             env("HELMR_WORKER_RESOURCE_ID", hostname()),
		WorkerRegion:                 envString("HELMR_WORKER_REGION"),
		WorkDir:                      envString("HELMR_WORKER_WORK_DIR"),
		ImagesDir:                    envString("HELMR_WORKER_IMAGES_DIR"),
		GitPath:                      env("HELMR_GIT_PATH", "git"),
		BuildKitAddr:                 envString("HELMR_WORKER_BUILDKIT_ADDR"),
		BuildKitCacheNS:              env("HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE", "helmr"),
		FirecrackerPath:              env("HELMR_WORKER_FIRECRACKER_PATH", "firecracker"),
		JailerPath:                   env("HELMR_WORKER_FIRECRACKER_JAILER_PATH", "jailer"),
		JailerNumaNode:               0,
		JailerChrootDir:              envString("HELMR_WORKER_FIRECRACKER_CHROOT_DIR"),
		CgroupVersion:                env("HELMR_WORKER_FIRECRACKER_CGROUP_VERSION", "2"),
		CNINetworkName:               env("HELMR_WORKER_CNI_NETWORK", "helmr"),
		CNIProfile:                   envString("HELMR_WORKER_CNI_PROFILE"),
		CNIConfDir:                   env("HELMR_WORKER_CNI_CONF_DIR", "/etc/cni/conf.d"),
		CNIBinDir:                    env("HELMR_WORKER_CNI_BIN_DIR", "/opt/cni/bin"),
		CNICacheDir:                  envString("HELMR_WORKER_CNI_CACHE_DIR"),
		IPPath:                       env("HELMR_WORKER_IP_PATH", "ip"),
		NFTPath:                      env("HELMR_WORKER_NFT_PATH", "nft"),
		NetworkBlockedIPv4CIDRs:      envList("HELMR_WORKER_NETWORK_BLOCKED_IPV4_CIDRS"),
		NetworkBlockedIPv6CIDRs:      envList("HELMR_WORKER_NETWORK_BLOCKED_IPV6_CIDRS"),
		VMVCPUCount:                  2,
		VMMemoryMiB:                  2048,
		VMScratchDiskMiB:             8192,
		VMHealthTimeout:              30 * time.Second,
		VMHealthAttemptTimeout:       5 * time.Second,
		WorkspaceMountStartupTimeout: 20 * time.Minute,
		PreparedBasePoolSize:         0,
		PreparedRuntimePoolSize:      0,
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
	if cfg.WorkerCapacityVCPUs, err = envInt64("HELMR_WORKER_CAPACITY_VCPUS", cfg.WorkerCapacityVCPUs); err != nil {
		return cfg, err
	}
	if cfg.WorkerCapacityVCPUs == 0 {
		cfg.WorkerCapacityVCPUs = cfg.VMVCPUCount
	}
	if cfg.WorkerCapacityVCPUs < cfg.VMVCPUCount {
		return cfg, errors.New("HELMR_WORKER_CAPACITY_VCPUS must be at least HELMR_VM_VCPUS")
	}
	if cfg.WorkerCapacityMemoryMiB, err = envInt64("HELMR_WORKER_CAPACITY_MEMORY_MIB", cfg.WorkerCapacityMemoryMiB); err != nil {
		return cfg, err
	}
	if cfg.WorkerCapacityMemoryMiB == 0 {
		cfg.WorkerCapacityMemoryMiB = cfg.VMMemoryMiB
	}
	if cfg.WorkerCapacityMemoryMiB < cfg.VMMemoryMiB {
		return cfg, errors.New("HELMR_WORKER_CAPACITY_MEMORY_MIB must be at least HELMR_VM_MEMORY_MIB")
	}
	if cfg.WorkerDiskMiB, err = envInt64("HELMR_WORKER_DISK_MIB", cfg.WorkerDiskMiB); err != nil {
		return cfg, err
	}
	if cfg.WorkerDiskMiB < 0 {
		return cfg, errors.New("HELMR_WORKER_DISK_MIB must be non-negative")
	}
	if cfg.SubstrateCacheMaxMiB, err = envInt64("HELMR_WORKER_SUBSTRATE_CACHE_MAX_MIB", cfg.SubstrateCacheMaxMiB); err != nil {
		return cfg, err
	}
	if cfg.SubstrateCacheMaxMiB < 0 {
		return cfg, errors.New("HELMR_WORKER_SUBSTRATE_CACHE_MAX_MIB must be non-negative")
	}
	if cfg.ArtifactCacheMaxMiB, err = envInt64("HELMR_WORKER_ARTIFACT_CACHE_MAX_MIB", cfg.ArtifactCacheMaxMiB); err != nil {
		return cfg, err
	}
	if cfg.ArtifactCacheMaxMiB < 0 {
		return cfg, errors.New("HELMR_WORKER_ARTIFACT_CACHE_MAX_MIB must be non-negative")
	}
	var workerExecutionSlots int
	if workerExecutionSlots, err = envInt("HELMR_WORKER_EXECUTION_SLOTS", int(cfg.WorkerExecutionSlots)); err != nil {
		return cfg, err
	}
	if workerExecutionSlots == 0 {
		workerExecutionSlots = 1
	}
	if workerExecutionSlots < 0 {
		return cfg, errors.New("HELMR_WORKER_EXECUTION_SLOTS must be positive")
	}
	if workerExecutionSlots > 1<<31-1 {
		return cfg, errors.New("HELMR_WORKER_EXECUTION_SLOTS must fit in int32")
	}
	cfg.WorkerExecutionSlots = int32(workerExecutionSlots)
	if cfg.VMHealthTimeout, err = envDuration("HELMR_VM_HEALTH_TIMEOUT", cfg.VMHealthTimeout); err != nil {
		return cfg, err
	}
	if cfg.VMHealthTimeout <= 0 {
		return cfg, errors.New("HELMR_VM_HEALTH_TIMEOUT must be positive")
	}
	healthAttemptTimeoutExplicit := envString("HELMR_VM_HEALTH_ATTEMPT_TIMEOUT") != ""
	if cfg.VMHealthAttemptTimeout, err = envDuration("HELMR_VM_HEALTH_ATTEMPT_TIMEOUT", cfg.VMHealthAttemptTimeout); err != nil {
		return cfg, err
	}
	if !healthAttemptTimeoutExplicit && cfg.VMHealthAttemptTimeout > cfg.VMHealthTimeout {
		cfg.VMHealthAttemptTimeout = cfg.VMHealthTimeout
	}
	if cfg.VMHealthAttemptTimeout <= 0 {
		return cfg, errors.New("HELMR_VM_HEALTH_ATTEMPT_TIMEOUT must be positive")
	}
	if cfg.VMHealthAttemptTimeout > cfg.VMHealthTimeout {
		return cfg, errors.New("HELMR_VM_HEALTH_ATTEMPT_TIMEOUT must be less than or equal to HELMR_VM_HEALTH_TIMEOUT")
	}
	if cfg.WorkspaceMountStartupTimeout, err = envDuration("HELMR_WORKSPACE_MOUNT_STARTUP_TIMEOUT", cfg.WorkspaceMountStartupTimeout); err != nil {
		return cfg, err
	}
	if cfg.WorkspaceMountStartupTimeout <= 0 {
		return cfg, errors.New("HELMR_WORKSPACE_MOUNT_STARTUP_TIMEOUT must be positive")
	}
	if cfg.PreparedBasePoolSize, err = envInt("HELMR_WORKER_PREPARED_BASE_POOL_SIZE", cfg.PreparedBasePoolSize); err != nil {
		return cfg, err
	}
	if cfg.PreparedBasePoolSize < 0 {
		return cfg, errors.New("HELMR_WORKER_PREPARED_BASE_POOL_SIZE must be non-negative")
	}
	if cfg.PreparedRuntimePoolSize, err = envInt("HELMR_WORKER_PREPARED_RUNTIME_POOL_SIZE", cfg.PreparedRuntimePoolSize); err != nil {
		return cfg, err
	}
	if cfg.PreparedRuntimePoolSize < 0 {
		return cfg, errors.New("HELMR_WORKER_PREPARED_RUNTIME_POOL_SIZE must be non-negative")
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
	value := envString(name)
	if value == "" {
		return map[string]string{}, nil
	}
	labels := map[string]string{}
	for part := range strings.SplitSeq(value, ",") {
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
		ControlURL:                   envString("HELMR_CONTROL_URL"),
		WorkerInstanceCredentialPath: envString("HELMR_WORKER_INSTANCE_CREDENTIAL_PATH"),
		WorkDir:                      envString("HELMR_WORKER_WORK_DIR"),
		PollEvery:                    2 * time.Second,
	}
	if cfg.ControlURL == "" {
		return cfg, errors.New("HELMR_CONTROL_URL is required")
	}
	return cfg, nil
}
