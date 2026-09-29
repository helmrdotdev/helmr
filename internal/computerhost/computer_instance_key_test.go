package computerhost

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestComputerInstanceIDFromComputerMount(t *testing.T) {
	mount := workerapi.ComputerInstanceAssignment{ComputerInstanceID: "019b4c0d-98a7-7b47-b29a-335824512378"}
	if got := computerInstanceIDFromComputerMount(mount); got != mount.ComputerInstanceID {
		t.Fatalf("runtime instance id = %q, want %q", got, mount.ComputerInstanceID)
	}
}
