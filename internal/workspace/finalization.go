package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const FinalizationFingerprintDomain = "helmr.workspace-finalization.v0\x00"
const FinalizationCaptureKind = "capture"

type FinalizationFence struct {
	WorkerInstanceID       string `json:"worker_instance_id"`
	WorkerEpoch            int64  `json:"worker_epoch"`
	RuntimeInstanceID      string `json:"runtime_instance_id"`
	RuntimeIdentityID      string `json:"runtime_identity_id"`
	WorkspaceID            string `json:"workspace_id"`
	WorkspaceMountID       string `json:"workspace_mount_id"`
	RunID                  string `json:"run_id"`
	AttemptNumber          uint32 `json:"attempt_number"`
	RunLeaseID             string `json:"run_lease_id"`
	LeaseSequence          int64  `json:"lease_sequence"`
	WorkspaceLeaseID       string `json:"workspace_lease_id"`
	OwnershipGeneration    int64  `json:"ownership_generation"`
	WriterGeneration       int64  `json:"writer_generation"`
	MountFencingGeneration int64  `json:"mount_fencing_generation"`
	ExpiresAtUnixNano      int64  `json:"expires_at_unix_nano"`
	BaseWorkspaceVersionID string `json:"base_workspace_version_id"`
}

type FinalizationRequest struct {
	OperationID string            `json:"operation_id"`
	Fence       FinalizationFence `json:"fence"`
	Target      any               `json:"target,omitempty"`
}

func FinalizationFingerprint(kind string, request FinalizationRequest) (string, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" || strings.TrimSpace(request.OperationID) == "" {
		return "", errors.New("Workspace finalization kind and operation ID are required")
	}
	body, err := json.Marshal(struct {
		Kind    string              `json:"kind"`
		Request FinalizationRequest `json:"request"`
	}{Kind: kind, Request: request})
	if err != nil {
		return "", err
	}
	canonical, err := jsoncanon.Transform(body)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(FinalizationFingerprintDomain))
	_, _ = hash.Write(canonical)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
