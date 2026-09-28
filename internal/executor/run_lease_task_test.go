package executor

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWaitComputerForRunUsesCurrentClaimFrontier(t *testing.T) {
	mount := workerapi.ComputerInstanceAssignment{
		ComputerID: "computer-1", ComputerMountPath: "/computer",
		WriterGeneration: 4,
		Target: workerapi.ComputerMountTarget{
			BaseComputerDiskVersionID: "version-before-capture",
		},
	}
	target := workerapi.ComputerMountTarget{
		BaseComputerDiskVersionID: "version-after-capture",
	}

	got := waitComputerForRun(mount, target)
	if got.ID != mount.ComputerID || got.ComputerInstanceID != mount.ComputerInstanceID || got.MountPath != mount.ComputerMountPath {
		t.Fatalf("wait Computer physical identity = %+v", got)
	}
	if got.WriterGeneration != mount.WriterGeneration || got.BaseComputerDiskVersionID != target.BaseComputerDiskVersionID || got.Artifact != nil {
		t.Fatalf("wait Computer logical frontier = %+v", got)
	}
}
