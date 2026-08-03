package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const FinalizationFingerprintDomain = "helmr.workspace-finalization.v0\x00"
const FinalizationCaptureKind = "capture"
const FinalizationResetKind = "reset"

const (
	ResetTargetEmpty    = "empty"
	ResetTargetArtifact = "artifact"
)

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

type ArtifactIdentity struct {
	Digest     string `json:"digest"`
	MediaType  string `json:"media_type"`
	Encoding   string `json:"encoding"`
	SizeBytes  int64  `json:"size_bytes"`
	EntryCount int    `json:"entry_count"`
}

type ResetTarget struct {
	Kind          string            `json:"kind"`
	BaseVersionID string            `json:"base_version_id"`
	Tree          TreeIdentity      `json:"tree"`
	Artifact      *ArtifactIdentity `json:"artifact,omitempty"`
}

func EmptyResetTarget(baseVersionID string, tree TreeIdentity) (ResetTarget, error) {
	target := ResetTarget{Kind: ResetTargetEmpty, BaseVersionID: strings.TrimSpace(baseVersionID), Tree: tree}
	if err := ValidateResetTarget(target); err != nil {
		return ResetTarget{}, err
	}
	return target, nil
}

func ArtifactResetTarget(baseVersionID string, tree TreeIdentity, artifact ArtifactIdentity) (ResetTarget, error) {
	target := ResetTarget{Kind: ResetTargetArtifact, BaseVersionID: strings.TrimSpace(baseVersionID), Tree: tree, Artifact: &artifact}
	if err := ValidateResetTarget(target); err != nil {
		return ResetTarget{}, err
	}
	return target, nil
}

func ValidateResetTarget(target ResetTarget) error {
	if strings.TrimSpace(target.BaseVersionID) == "" {
		return errors.New("Workspace Reset base version ID is required")
	}
	if !sha256sum.ValidDigest(target.Tree.Digest) || target.Tree.SizeBytes < 0 || target.Tree.SizeBytes > MaxArtifactExtractedBytes || target.Tree.EntryCount < 0 || target.Tree.EntryCount > MaxArtifactEntries {
		return errors.New("Workspace Reset tree identity is invalid")
	}
	switch target.Kind {
	case ResetTargetEmpty:
		if target.Artifact != nil || target.Tree.Digest != CanonicalEmptyTreeDigest || target.Tree.SizeBytes != 0 || target.Tree.EntryCount != 0 {
			return errors.New("empty Workspace Reset target must be the canonical empty tree")
		}
	case ResetTargetArtifact:
		if target.Artifact == nil || !sha256sum.ValidDigest(target.Artifact.Digest) || target.Artifact.MediaType != ArtifactMediaType || target.Artifact.Encoding != ArtifactEncoding || target.Artifact.SizeBytes <= 0 || target.Artifact.SizeBytes > MaxArtifactArchiveBytes || target.Artifact.EntryCount < 0 || target.Artifact.EntryCount > MaxArtifactEntries {
			return errors.New("Workspace Reset Artifact descriptor is invalid")
		}
	default:
		return errors.New("Workspace Reset target kind is invalid")
	}
	return nil
}

func ValidateTreeIdentity(tree TreeIdentity) error {
	if !sha256sum.ValidDigest(tree.Digest) || tree.SizeBytes < 0 || tree.SizeBytes > MaxArtifactExtractedBytes ||
		tree.EntryCount < 0 || tree.EntryCount > MaxArtifactEntries {
		return errors.New("Workspace tree identity is invalid")
	}
	return nil
}

func ResetTargetsEqual(left, right ResetTarget) bool {
	if left.Kind != right.Kind || left.BaseVersionID != right.BaseVersionID || left.Tree != right.Tree {
		return false
	}
	if left.Artifact == nil || right.Artifact == nil {
		return left.Artifact == nil && right.Artifact == nil
	}
	return *left.Artifact == *right.Artifact
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
