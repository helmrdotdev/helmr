package executor

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestRuntimeInstanceIDFromWorkspaceMount(t *testing.T) {
	mount := api.WorkerWorkspaceMount{RuntimeInstanceID: "019b4c0d-98a7-7b47-b29a-335824512378"}
	if got := runtimeInstanceIDFromWorkspaceMount(mount); got != mount.RuntimeInstanceID {
		t.Fatalf("runtime instance id = %q, want %q", got, mount.RuntimeInstanceID)
	}
}
