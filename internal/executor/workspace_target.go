package executor

import (
	"errors"
	"strings"

	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func workspaceTargetFromWorker(target workerapi.WorkspaceResetTarget) (workspace.ResetTarget, error) {
	tree := workspace.TreeIdentity{
		Digest: strings.TrimSpace(target.Tree.Digest), SizeBytes: target.Tree.SizeBytes,
		EntryCount: int(target.Tree.EntryCount),
	}
	switch {
	case target.Empty != nil && target.Artifact == nil:
		return workspace.EmptyResetTarget(target.BaseWorkspaceVersionID, tree)
	case target.Empty == nil && target.Artifact != nil:
		artifact := target.Artifact
		return workspace.ArtifactResetTarget(target.BaseWorkspaceVersionID, tree, workspace.ArtifactIdentity{
			Digest: strings.TrimSpace(artifact.Digest), MediaType: artifact.MediaType,
			Encoding: artifact.Encoding, SizeBytes: artifact.SizeBytes,
			EntryCount: int(artifact.EntryCount),
		})
	default:
		return workspace.ResetTarget{}, errors.New("workspace target must contain exactly one source")
	}
}

func workspaceResetTargetProto(target workerapi.WorkspaceResetTarget) *workspacev0.WorkspaceResetTarget {
	projected, err := workspaceTargetFromWorker(target)
	if err != nil {
		return nil
	}
	return workspace.ResetTargetProto(projected)
}
