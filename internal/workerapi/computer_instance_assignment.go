package workerapi

import "time"

type ComputerInstanceClaimResponse struct {
	Assignment *ComputerInstanceAssignment `json:"assignment,omitempty"`
}

type ComputerInstanceAssignment struct {
	DesiredVersion             int64               `json:"desired_version"`
	ObservedVersion            int64               `json:"observed_version"`
	WriterGeneration           int64               `json:"writer_generation"`
	OrgID                      string              `json:"org_id"`
	ProjectID                  string              `json:"project_id"`
	EnvironmentID              string              `json:"environment_id"`
	ComputerID                 string              `json:"computer_id"`
	ComputerSpecID             string              `json:"computer_spec_id"`
	Target                     ComputerMountTarget `json:"target"`
	ComputerInstanceID         string              `json:"computer_instance_id,omitempty"`
	RestoreCheckpointID        string              `json:"restore_checkpoint_id,omitempty"`
	WorkerEpoch                int64               `json:"worker_epoch"`
	GuestChannelCredential     string              `json:"guest_channel_credential"`
	GuestChannelCredentialHash string              `json:"guest_channel_credential_hash"`
	VMPlatformID               string              `json:"vm_platform_id"`
	ComputerImage              CASObject           `json:"computer_image"`
	RootfsDigest               string              `json:"rootfs_digest"`
	ComputerMountPath          string              `json:"computer_mount_path"`
	RequestedMilliCPU          int64               `json:"requested_milli_cpu"`
	RequestedMemoryMiB         int64               `json:"requested_memory_mib"`
	RequestedDiskMiB           int64               `json:"requested_disk_mib"`
	RequestedExecutionSlots    int32               `json:"requested_execution_slots"`
	VMRuntimeContract          string              `json:"vm_runtime_contract"`
	ExpiresAt                  time.Time           `json:"expires_at"`
}
