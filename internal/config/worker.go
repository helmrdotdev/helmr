package config

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

func LoadWorker() (Worker, error) {
	cfg := Worker{
		ControlPlaneURL:              envText("CONTROL_PLANE_URL"),
		WorkerResourceID:             envText("WORKER_RESOURCE_ID"),
		WorkerEnrollmentTokenFile:    envText("WORKER_ENROLLMENT_TOKEN_FILE"),
		CASURI:                       envText("CAS_URI"),
		WorkerInstanceCredentialPath: envText("WORKER_INSTANCE_CREDENTIAL_PATH"),
		BuildPolicyPath:              envText("BUILD_POLICY_PATH"),
		PlatformStoreURI:             envText("PLATFORM_STORE_URI"),
		WorkDir:                      envText("WORKER_WORK_DIR"),
		BuildCacheDir:                envText("WORKER_BUILD_CACHE_DIR"),
		BuildScratchDir:              envText("WORKER_BUILD_SCRATCH_DIR"),
		ImagesDir:                    envText("WORKER_IMAGES_DIR"),
		FirecrackerPath:              env("FIRECRACKER_PATH", "firecracker"),
		JailerPath:                   env("JAILER_PATH", "jailer"),
		JailerNumaNode:               0,
		JailerChrootDir:              envText("JAILER_CHROOT_DIR"),
		CgroupVersion:                env("JAILER_CGROUP_VERSION", "2"),
		NetworkLinkPool:              envText("WORKER_NETWORK_LINK_POOL"),
		NetworkTranslationPool:       envText("WORKER_NETWORK_TRANSLATION_POOL"),
		NetworkResolverIPv4:          envText("WORKER_NETWORK_RESOLVER_IPV4"),
		IPPath:                       env("IP_PATH", "ip"),
		NFTPath:                      env("NFT_PATH", "nft"),
		VMVCPUCount:                  2,
		VMMemoryMiB:                  2048,
		VMScratchDiskMiB:             8192,
		WorkerDiskReserveMiB:         1024,
		VMInitTimeout:                30 * time.Second,
		VMHealthTimeout:              30 * time.Second,
		VMHealthAttemptTimeout:       5 * time.Second,
		WorkspaceMountStartupTimeout: 20 * time.Minute,
		PreparedRuntimePoolSize:      0,
		PollEvery:                    2 * time.Second,
	}
	if cfg.WorkerResourceID == "" || len(cfg.WorkerResourceID) > 512 {
		return cfg, errors.New("WORKER_RESOURCE_ID is required and must not exceed 512 bytes")
	}
	if cfg.WorkerEnrollmentTokenFile == "" {
		return cfg, errors.New("WORKER_ENROLLMENT_TOKEN_FILE is required")
	}
	var err error
	if cfg.ImageCache, err = loadImageCache(); err != nil {
		return cfg, err
	}
	cfg.NetworkBlockedIPv4CIDRs, err = parseCanonicalBlockedIPv4Prefixes(
		envText("WORKER_NETWORK_BLOCKED_IPV4_CIDRS"),
	)
	if err != nil {
		return cfg, fmt.Errorf("WORKER_NETWORK_BLOCKED_IPV4_CIDRS: %w", err)
	}
	if cfg.VMVCPUCount, err = envInt64("VM_VCPUS", cfg.VMVCPUCount); err != nil {
		return cfg, err
	}
	if cfg.VMMemoryMiB, err = envInt64("VM_MEMORY_MIB", cfg.VMMemoryMiB); err != nil {
		return cfg, err
	}
	if cfg.VMScratchDiskMiB, err = envInt64("VM_SCRATCH_DISK_MIB", cfg.VMScratchDiskMiB); err != nil {
		return cfg, err
	}
	if cfg.VMScratchDiskMiB <= 0 {
		return cfg, errors.New("VM_SCRATCH_DISK_MIB must be positive")
	}
	if cfg.WorkerCapacityVCPUs, err = envInt64("WORKER_CAPACITY_VCPUS", cfg.WorkerCapacityVCPUs); err != nil {
		return cfg, err
	}
	if cfg.WorkerCapacityVCPUs == 0 {
		cfg.WorkerCapacityVCPUs = cfg.VMVCPUCount
	}
	if cfg.WorkerCapacityVCPUs < cfg.VMVCPUCount {
		return cfg, errors.New("WORKER_CAPACITY_VCPUS must be at least VM_VCPUS")
	}
	if cfg.WorkerCapacityMemoryMiB, err = envInt64("WORKER_CAPACITY_MEMORY_MIB", cfg.WorkerCapacityMemoryMiB); err != nil {
		return cfg, err
	}
	if cfg.WorkerCapacityMemoryMiB == 0 {
		cfg.WorkerCapacityMemoryMiB = cfg.VMMemoryMiB
	}
	if cfg.WorkerCapacityMemoryMiB < cfg.VMMemoryMiB {
		return cfg, errors.New("WORKER_CAPACITY_MEMORY_MIB must be at least VM_MEMORY_MIB")
	}
	if cfg.WorkerDiskMiB, err = envInt64("WORKER_DISK_MIB", cfg.WorkerDiskMiB); err != nil {
		return cfg, err
	}
	if cfg.WorkerDiskMiB < 0 {
		return cfg, errors.New("WORKER_DISK_MIB must be non-negative")
	}
	if cfg.WorkerDiskReserveMiB, err = envInt64("WORKER_DISK_RESERVE_MIB", cfg.WorkerDiskReserveMiB); err != nil {
		return cfg, err
	}
	if cfg.WorkerDiskReserveMiB <= 0 {
		return cfg, errors.New("WORKER_DISK_RESERVE_MIB must be positive")
	}
	if cfg.SubstrateCacheMaxMiB, err = envInt64("WORKER_SUBSTRATE_CACHE_MAX_MIB", cfg.SubstrateCacheMaxMiB); err != nil {
		return cfg, err
	}
	if cfg.SubstrateCacheMaxMiB < 0 {
		return cfg, errors.New("WORKER_SUBSTRATE_CACHE_MAX_MIB must be non-negative")
	}
	if cfg.ArtifactCacheMaxMiB, err = envInt64("WORKER_ARTIFACT_CACHE_MAX_MIB", cfg.ArtifactCacheMaxMiB); err != nil {
		return cfg, err
	}
	if cfg.ArtifactCacheMaxMiB < 0 {
		return cfg, errors.New("WORKER_ARTIFACT_CACHE_MAX_MIB must be non-negative")
	}
	var workerExecutionSlots int
	if workerExecutionSlots, err = envInt("WORKER_EXECUTION_SLOTS", int(cfg.WorkerExecutionSlots)); err != nil {
		return cfg, err
	}
	if workerExecutionSlots == 0 {
		workerExecutionSlots = 1
	}
	if workerExecutionSlots < 0 {
		return cfg, errors.New("WORKER_EXECUTION_SLOTS must be positive")
	}
	if workerExecutionSlots > 1<<31-1 {
		return cfg, errors.New("WORKER_EXECUTION_SLOTS must fit in int32")
	}
	cfg.WorkerExecutionSlots = int32(workerExecutionSlots)
	cfg.WorkerRoles, err = parseWorkerRoles(envText("WORKER_ROLES"))
	if err != nil {
		return cfg, err
	}
	var runtimeStarts int
	if runtimeStarts, err = envInt("WORKER_RUNTIME_STARTS", int(cfg.WorkerRuntimeStarts)); err != nil {
		return cfg, err
	}
	if runtimeStarts == 0 && slices.Contains(cfg.WorkerRoles, "run") {
		runtimeStarts = int(cfg.WorkerExecutionSlots)
	}
	if runtimeStarts < 0 || runtimeStarts > 1<<31-1 {
		return cfg, errors.New("WORKER_RUNTIME_STARTS must be non-negative and fit in int32")
	}
	if !slices.Contains(cfg.WorkerRoles, "run") && runtimeStarts != 0 {
		return cfg, errors.New("WORKER_RUNTIME_STARTS must be zero when run role is disabled")
	}
	cfg.WorkerRuntimeStarts = int32(runtimeStarts)
	if cfg.VMInitTimeout, err = envDuration("VM_INIT_TIMEOUT", cfg.VMInitTimeout); err != nil {
		return cfg, err
	}
	if cfg.VMInitTimeout <= 0 {
		return cfg, errors.New("VM_INIT_TIMEOUT must be positive")
	}
	if cfg.VMHealthTimeout, err = envDuration("VM_HEALTH_TIMEOUT", cfg.VMHealthTimeout); err != nil {
		return cfg, err
	}
	if cfg.VMHealthTimeout <= 0 {
		return cfg, errors.New("VM_HEALTH_TIMEOUT must be positive")
	}
	healthAttemptTimeoutExplicit := envText("VM_HEALTH_ATTEMPT_TIMEOUT") != ""
	if cfg.VMHealthAttemptTimeout, err = envDuration("VM_HEALTH_ATTEMPT_TIMEOUT", cfg.VMHealthAttemptTimeout); err != nil {
		return cfg, err
	}
	if !healthAttemptTimeoutExplicit && cfg.VMHealthAttemptTimeout > cfg.VMHealthTimeout {
		cfg.VMHealthAttemptTimeout = cfg.VMHealthTimeout
	}
	if cfg.VMHealthAttemptTimeout <= 0 {
		return cfg, errors.New("VM_HEALTH_ATTEMPT_TIMEOUT must be positive")
	}
	if cfg.VMHealthAttemptTimeout > cfg.VMHealthTimeout {
		return cfg, errors.New("VM_HEALTH_ATTEMPT_TIMEOUT must be less than or equal to VM_HEALTH_TIMEOUT")
	}
	if cfg.WorkspaceMountStartupTimeout, err = envDuration("WORKSPACE_MOUNT_STARTUP_TIMEOUT", cfg.WorkspaceMountStartupTimeout); err != nil {
		return cfg, err
	}
	if cfg.WorkspaceMountStartupTimeout <= 0 {
		return cfg, errors.New("WORKSPACE_MOUNT_STARTUP_TIMEOUT must be positive")
	}
	if cfg.PreparedRuntimePoolSize, err = envInt("WORKER_PREPARED_RUNTIME_POOL_SIZE", cfg.PreparedRuntimePoolSize); err != nil {
		return cfg, err
	}
	if cfg.PreparedRuntimePoolSize < 0 {
		return cfg, errors.New("WORKER_PREPARED_RUNTIME_POOL_SIZE must be non-negative")
	}
	if slices.Contains(cfg.WorkerRoles, "run") && cfg.PreparedRuntimePoolSize == 0 {
		cfg.PreparedRuntimePoolSize = int(cfg.WorkerRuntimeStarts)
	}
	if slices.Contains(cfg.WorkerRoles, "run") && cfg.PreparedRuntimePoolSize < int(cfg.WorkerRuntimeStarts) {
		return cfg, errors.New("WORKER_PREPARED_RUNTIME_POOL_SIZE must cover WORKER_RUNTIME_STARTS")
	}
	if cfg.JailerUID, err = envInt("JAILER_UID", cfg.JailerUID); err != nil {
		return cfg, err
	}
	if cfg.JailerGID, err = envInt("JAILER_GID", cfg.JailerGID); err != nil {
		return cfg, err
	}
	if cfg.JailerNumaNode, err = envInt("JAILER_NUMA_NODE", cfg.JailerNumaNode); err != nil {
		return cfg, err
	}
	if cfg.ControlPlaneURL == "" {
		return cfg, errors.New("CONTROL_PLANE_URL is required")
	}
	if cfg.CASURI == "" {
		return cfg, errors.New("CAS_URI is required")
	}
	if (slices.Contains(cfg.WorkerRoles, "run") ||
		slices.Contains(cfg.WorkerRoles, "build")) &&
		cfg.PlatformStoreURI == "" {
		return cfg, errors.New("PLATFORM_STORE_URI is required for run and build workers")
	}
	if slices.Contains(cfg.WorkerRoles, "build") && cfg.BuildPolicyPath == "" {
		return cfg, errors.New("BUILD_POLICY_PATH is required for build workers")
	}
	if slices.Contains(cfg.WorkerRoles, "build") && cfg.BuildCacheDir == "" {
		return cfg, errors.New("WORKER_BUILD_CACHE_DIR is required for build workers")
	}
	if slices.Contains(cfg.WorkerRoles, "build") && cfg.BuildScratchDir == "" {
		return cfg, errors.New("WORKER_BUILD_SCRATCH_DIR is required for build workers")
	}
	if slices.Contains(cfg.WorkerRoles, "build") && cfg.SubstrateCacheMaxMiB <= 0 {
		return cfg, errors.New("WORKER_SUBSTRATE_CACHE_MAX_MIB is required for build workers")
	}
	if slices.Contains(cfg.WorkerRoles, "build") && cfg.ArtifactCacheMaxMiB <= 0 {
		return cfg, errors.New("WORKER_ARTIFACT_CACHE_MAX_MIB is required for build workers")
	}
	if cfg.SubstrateCacheMaxMiB > math.MaxInt64/(1024*1024) ||
		cfg.ArtifactCacheMaxMiB > math.MaxInt64/(1024*1024) ||
		cfg.SubstrateCacheMaxMiB > math.MaxInt64-cfg.ArtifactCacheMaxMiB {
		return cfg, errors.New("worker cache capacity exceeds the supported byte range")
	}
	cfg.CheckpointKey, err = rootKey("CHECKPOINT_ENCRYPTION_KEY")
	if err != nil {
		return cfg, err
	}
	if cfg.JailerUID <= 0 {
		return cfg, errors.New("JAILER_UID is required")
	}
	if cfg.JailerGID <= 0 {
		return cfg, errors.New("JAILER_GID is required")
	}
	if cfg.NetworkLinkPool == "" {
		return cfg, errors.New("WORKER_NETWORK_LINK_POOL is required")
	}
	if cfg.NetworkTranslationPool == "" {
		return cfg, errors.New("WORKER_NETWORK_TRANSLATION_POOL is required")
	}
	if cfg.NetworkResolverIPv4 == "" {
		return cfg, errors.New("WORKER_NETWORK_RESOLVER_IPV4 is required")
	}
	return cfg, nil
}

func parseWorkerRoles(value string) ([]string, error) {
	seen := map[string]bool{}
	for part := range strings.SplitSeq(value, ",") {
		role := strings.ToLower(strings.TrimSpace(part))
		if role != "run" && role != "build" {
			return nil, fmt.Errorf("WORKER_ROLES contains unsupported role %q", role)
		}
		seen[role] = true
	}
	roles := make([]string, 0, 2)
	for _, role := range []string{"build", "run"} {
		if seen[role] {
			roles = append(roles, role)
		}
	}
	if len(roles) == 0 {
		return nil, errors.New("WORKER_ROLES must enable run, build, or both")
	}
	return roles, nil
}

func LoadWorkerControlPlane() (WorkerControlPlane, error) {
	cfg := WorkerControlPlane{
		ControlPlaneURL:              envText("CONTROL_PLANE_URL"),
		WorkerInstanceCredentialPath: envText("WORKER_INSTANCE_CREDENTIAL_PATH"),
		WorkDir:                      envText("WORKER_WORK_DIR"),
		PollEvery:                    2 * time.Second,
	}
	if cfg.ControlPlaneURL == "" {
		return cfg, errors.New("CONTROL_PLANE_URL is required")
	}
	return cfg, nil
}
