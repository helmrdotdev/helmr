package executor

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWaitWorkspaceForRunUsesCurrentClaimFrontier(t *testing.T) {
	mountArtifact := &workerapi.WorkspaceArtifact{Digest: "mount-artifact"}
	claimArtifact := &workerapi.WorkspaceArtifact{
		Digest: "claim-artifact", MediaType: "application/vnd.helmr.workspace.v1+tar",
		Encoding: "identity", SizeBytes: 17, EntryCount: 2,
	}
	mount := workerapi.WorkspaceMount{
		ID: "mount-1", WorkspaceID: "workspace-1", WorkspaceMountPath: "/workspace",
		FencingGeneration: 4,
		Target: workerapi.WorkspaceResetTarget{
			BaseWorkspaceVersionID: "version-before-capture",
			Artifact:               mountArtifact,
		},
	}
	lease := workerapi.RunLeaseAssignment{MountFencingGeneration: 9}
	target := workerapi.WorkspaceResetTarget{
		BaseWorkspaceVersionID: "version-after-capture",
		Artifact:               claimArtifact,
	}

	got := waitWorkspaceForRun(mount, lease, target)
	if got.ID != mount.WorkspaceID || got.WorkspaceMountID != mount.ID || got.MountPath != mount.WorkspaceMountPath {
		t.Fatalf("wait Workspace physical identity = %+v", got)
	}
	if got.FencingGeneration != lease.MountFencingGeneration || got.BaseVersionID != target.BaseWorkspaceVersionID || got.Artifact != claimArtifact {
		t.Fatalf("wait Workspace logical frontier = %+v", got)
	}
}
