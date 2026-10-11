//go:build !linux

package computerhost

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (p *PreparedMachines) stagePreparation(context.Context, *preparationStage, workerapi.AllocationIdentity, workerapi.PreparationStart) error {
	return errors.New("private preparation requires Linux")
}
