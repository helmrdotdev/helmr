package executor

import (
	"errors"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"strings"
)

func validateComputerMountTarget(target workerapi.ComputerMountTarget) error {
	if strings.TrimSpace(target.BaseComputerDiskVersionID) == "" {
		return errors.New("computer mount version is required")
	}
	return nil
}
func computerMountTargetProto(target workerapi.ComputerMountTarget) *computerv0.ComputerMountTarget {
	return &computerv0.ComputerMountTarget{BaseComputerDiskVersionId: target.BaseComputerDiskVersionID}
}
