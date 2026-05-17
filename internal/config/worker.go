package config

import (
	"errors"
	"os"
	"time"
)

func LoadWorker() (Worker, error) {
	cfg := Worker{
		ControlURL:                      os.Getenv("HELMR_CONTROL_URL"),
		CASURI:                          os.Getenv("HELMR_CAS_URI"),
		WorkerPoolRegistrationToken:     os.Getenv("HELMR_WORKER_POOL_REGISTRATION_TOKEN"),
		WorkerPoolRegistrationTokenPath: os.Getenv("HELMR_WORKER_POOL_REGISTRATION_TOKEN_PATH"),
		WorkerSecret:                    os.Getenv("HELMR_WORKER_SECRET"),
		WorkerCredentialPath:            os.Getenv("HELMR_WORKER_CREDENTIAL_PATH"),
		CheckpointKey:                   os.Getenv("HELMR_CHECKPOINT_ENCRYPTION_KEY"),
		WorkerID:                        env("HELMR_WORKER_ID", hostname()),
		WorkDir:                         os.Getenv("HELMR_WORKER_WORK_DIR"),
		ImagesDir:                       os.Getenv("HELMR_WORKER_IMAGES_DIR"),
		GitPath:                         env("HELMR_GIT_PATH", "git"),
		BuildKitAddr:                    os.Getenv("HELMR_WORKER_BUILDKIT_ADDR"),
		BuildKitCacheNS:                 env("HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE", "helmr"),
		FirecrackerPath:                 env("HELMR_WORKER_FIRECRACKER_PATH", "firecracker"),
		JailerPath:                      env("HELMR_WORKER_FIRECRACKER_JAILER_PATH", "jailer"),
		JailerNumaNode:                  0,
		JailerChrootDir:                 os.Getenv("HELMR_WORKER_FIRECRACKER_CHROOT_DIR"),
		CgroupVersion:                   env("HELMR_WORKER_FIRECRACKER_CGROUP_VERSION", "2"),
		CNINetworkName:                  env("HELMR_WORKER_CNI_NETWORK", "helmr"),
		CNIProfile:                      os.Getenv("HELMR_WORKER_CNI_PROFILE"),
		CNIConfDir:                      env("HELMR_WORKER_CNI_CONF_DIR", "/etc/cni/conf.d"),
		CNIBinDir:                       env("HELMR_WORKER_CNI_BIN_DIR", "/opt/cni/bin"),
		CNICacheDir:                     os.Getenv("HELMR_WORKER_CNI_CACHE_DIR"),
		IPPath:                          env("HELMR_WORKER_IP_PATH", "ip"),
		NFTPath:                         env("HELMR_WORKER_NFT_PATH", "nft"),
		NetworkBlockedIPv4CIDRs:         envList("HELMR_WORKER_NETWORK_BLOCKED_IPV4_CIDRS"),
		NetworkBlockedIPv6CIDRs:         envList("HELMR_WORKER_NETWORK_BLOCKED_IPV6_CIDRS"),
		VMVCPUCount:                     2,
		VMMemoryMiB:                     2048,
		VMHealthTimeout:                 30 * time.Second,
		PollEvery:                       2 * time.Second,
	}
	var err error
	if cfg.VMVCPUCount, err = envInt64("HELMR_VM_VCPUS", cfg.VMVCPUCount); err != nil {
		return cfg, err
	}
	if cfg.VMMemoryMiB, err = envInt64("HELMR_VM_MEMORY_MIB", cfg.VMMemoryMiB); err != nil {
		return cfg, err
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
		cfg.CNIProfile = cfg.CNINetworkName + "/v1"
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

func LoadWorkerControl() (WorkerControl, error) {
	cfg := WorkerControl{
		ControlURL:           os.Getenv("HELMR_CONTROL_URL"),
		WorkerSecret:         os.Getenv("HELMR_WORKER_SECRET"),
		WorkerCredentialPath: os.Getenv("HELMR_WORKER_CREDENTIAL_PATH"),
		WorkerID:             env("HELMR_WORKER_ID", hostname()),
		WorkDir:              os.Getenv("HELMR_WORKER_WORK_DIR"),
		PollEvery:            2 * time.Second,
	}
	if cfg.ControlURL == "" {
		return cfg, errors.New("HELMR_CONTROL_URL is required")
	}
	return cfg, nil
}
