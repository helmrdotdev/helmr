package workspace

import (
	"errors"
	"math"
	"strings"

	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
)

func ResetTargetFromProto(target *workspacev0.WorkspaceResetTarget) (ResetTarget, error) {
	if target == nil || target.GetTree() == nil {
		return ResetTarget{}, errors.New("Workspace Reset target is required")
	}
	tree := TreeIdentity{
		Digest:     strings.TrimSpace(target.GetTree().GetDigest()),
		SizeBytes:  target.GetTree().GetSizeBytes(),
		EntryCount: int(target.GetTree().GetEntryCount()),
	}
	switch source := target.GetSource().(type) {
	case *workspacev0.WorkspaceResetTarget_Empty:
		if source.Empty == nil {
			return ResetTarget{}, errors.New("Workspace Reset empty target is required")
		}
		return EmptyResetTarget(target.GetBaseVersionId(), tree)
	case *workspacev0.WorkspaceResetTarget_Artifact:
		if source.Artifact == nil || source.Artifact.GetSizeBytes() > math.MaxInt64 {
			return ResetTarget{}, errors.New("Workspace Reset Artifact target is required")
		}
		return ArtifactResetTarget(target.GetBaseVersionId(), tree, ArtifactIdentity{
			Digest:     strings.TrimSpace(source.Artifact.GetDigest()),
			MediaType:  source.Artifact.GetMediaType(),
			Encoding:   source.Artifact.GetEncoding(),
			SizeBytes:  int64(source.Artifact.GetSizeBytes()),
			EntryCount: int(source.Artifact.GetEntryCount()),
		})
	default:
		return ResetTarget{}, errors.New("Workspace Reset target source is required")
	}
}

func ResetTargetProto(target ResetTarget) *workspacev0.WorkspaceResetTarget {
	result := &workspacev0.WorkspaceResetTarget{
		BaseVersionId: target.BaseVersionID,
		Tree: &workspacev0.WorkspaceTreeIdentity{
			Digest:     target.Tree.Digest,
			SizeBytes:  target.Tree.SizeBytes,
			EntryCount: uint32(target.Tree.EntryCount),
		},
	}
	if target.Kind == ResetTargetEmpty {
		result.Source = &workspacev0.WorkspaceResetTarget_Empty{Empty: &workspacev0.EmptyWorkspaceResetTarget{}}
	} else {
		result.Source = &workspacev0.WorkspaceResetTarget_Artifact{Artifact: &workspacev0.WorkspaceArtifact{
			Digest:     target.Artifact.Digest,
			MediaType:  target.Artifact.MediaType,
			Encoding:   target.Artifact.Encoding,
			SizeBytes:  uint64(target.Artifact.SizeBytes),
			EntryCount: uint32(target.Artifact.EntryCount),
		}}
	}
	return result
}
