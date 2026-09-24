package executor

import (
	"errors"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"strings"
)

func validateComputerMountTarget(target workerapi.ComputerMountTarget) error {
	if strings.TrimSpace(target.BaseWorkspaceVersionID) == "" {
		return errors.New("computer mount version is required")
	}
	return nil
}
func computerMountTargetProto(target workerapi.ComputerMountTarget) *workspacev0.ComputerMountTarget {
	return &workspacev0.ComputerMountTarget{BaseWorkspaceVersionId: target.BaseWorkspaceVersionID}
}
