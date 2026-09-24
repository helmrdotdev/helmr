package executor

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWaitWorkspaceForRunUsesCurrentClaimFrontier(t *testing.T) {
	mount := workerapi.WorkspaceMount{
		ID: "mount-1", WorkspaceID: "workspace-1", WorkspaceMountPath: "/workspace",
		FencingGeneration: 4,
		Target: workerapi.ComputerMountTarget{
			BaseWorkspaceVersionID: "version-before-capture",
		},
	}
	lease := workerapi.RunLeaseAssignment{MountFencingGeneration: 9}
	target := workerapi.ComputerMountTarget{
		BaseWorkspaceVersionID: "version-after-capture",
	}

	got := waitWorkspaceForRun(mount, lease, target)
	if got.ID != mount.WorkspaceID || got.WorkspaceMountID != mount.ID || got.MountPath != mount.WorkspaceMountPath {
		t.Fatalf("wait Workspace physical identity = %+v", got)
	}
	if got.FencingGeneration != lease.MountFencingGeneration || got.BaseWorkspaceVersionID != target.BaseWorkspaceVersionID || got.Artifact != nil {
		t.Fatalf("wait Workspace logical frontier = %+v", got)
	}
}
