package executor

import (
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"testing"
)

func TestCheckpointBaseUsesPreparedComputer(t *testing.T) {
	base, err := checkpointComputerBase(workerapi.ComputerMountTarget{BaseComputerDiskVersionID: "version-1"})
	if err != nil {
		t.Fatal(err)
	}
	if base.MountPath != "/workspace" {
		t.Fatalf("base=%+v", base)
	}
	if _, err := checkpointComputerBase(workerapi.ComputerMountTarget{}); err == nil {
		t.Fatal("missing version accepted")
	}
}
