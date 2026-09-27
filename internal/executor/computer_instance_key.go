package executor

import "github.com/helmrdotdev/helmr/internal/workerapi"

func computerInstanceIDFromComputerMount(mount workerapi.ComputerInstanceAssignment) string {
	return mount.ComputerInstanceID
}
