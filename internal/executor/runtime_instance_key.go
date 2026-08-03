package executor

import "github.com/helmrdotdev/helmr/internal/workerapi"

func runtimeInstanceIDFromWorkspaceMount(mount workerapi.WorkspaceMount) string {
	return mount.RuntimeInstanceID
}
