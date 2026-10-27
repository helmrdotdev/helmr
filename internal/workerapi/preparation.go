package workerapi

import (
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"time"

	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

// PreparationExecutor addresses the allocated private executor. The credential
// binds the private guest channel; CP operations also require the owning Host.
type PreparationExecutor struct {
	Network           *NetworkCapabilities `json:"network,omitempty"`
	Identity          AllocationIdentity   `json:"identity"`
	ChannelCredential []byte               `json:"channel_credential"`
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
	Executor    PreparationExecutor            `json:"executor"`
	Inspections []blockformat.ObjectInspection `json:"inspections"`
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
	NetworkPolicy        networkpolicy.Policy            `json:"network_policy"`
	NetworkInstallation  networkpolicy.Installation      `json:"network_installation"`
	SeedOnly             bool                            `json:"seed_only"`
	Program              RuntimeProgram                  `json:"program"`
	TemplateDefinitionID string                          `json:"template_definition_id"`
	SeedObject           CASObject                       `json:"seed_object"`
	Seed                 definition.ComputerSeedManifest `json:"seed"`
	RootfsDigest         string                          `json:"rootfs_digest"`
}
