package executor

import (
	"errors"
	"strings"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func validateComputerMountTarget(target workerapi.ComputerMountTarget) error {
	if strings.TrimSpace(target.BaseComputerDiskVersionID) == "" {
		return errors.New("computer mount version is required")
	}
	return nil
}
