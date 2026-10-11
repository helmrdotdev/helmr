package workerapi

import (
	"time"

	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

// PreparationExecutor addresses the allocated private executor. The credential
// binds the private guest channel; CP operations also require the owning Host.
type PreparationExecutor struct {
	Identity          AllocationIdentity `json:"identity"`
	ChannelCredential []byte             `json:"channel_credential"`
}
type PreparationRenewal struct {
	ExpiresAt time.Time `json:"expires_at"`
}
type PreparationFailure struct {
	Executor PreparationExecutor `json:"executor"`
	Code     string              `json:"code"`
}
type PreparationCaptureBegin struct {
	Executor     PreparationExecutor `json:"executor"`
	LogicalBytes int64               `json:"logical_bytes"`
}
type PreparationObject struct {
	Executor   PreparationExecutor          `json:"executor"`
	Inspection blockformat.ObjectInspection `json:"inspection"`
}
type PreparationPublication struct {
	Executor PreparationExecutor `json:"executor"`
	Root     disk.VersionRoot    `json:"root"`
	Evidence string              `json:"evidence"`
}

type PreparationSecrets struct {
	Secrets   []SecretDelivery `json:"secrets"`
	Protected *ProtectedEnv    `json:"protected,omitempty"`
}

type PreparationStart struct {
	Program              RuntimeProgram                  `json:"program"`
	ComputerDefinitionID string                          `json:"computer_definition_id"`
	SeedObject           CASObject                       `json:"seed_object"`
	Seed                 definition.ComputerSeedManifest `json:"seed"`
	RootfsDigest         string                          `json:"rootfs_digest"`
}
