package executor

import "github.com/helmrdotdev/helmr/internal/api"

func runtimeInstanceIDFromWorkspaceMount(mount api.WorkerWorkspaceMount) string {
	return mount.RuntimeInstanceID
}
