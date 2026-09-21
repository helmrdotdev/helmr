//go:build !linux

package executor

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"os"
)

func (p *PreparedRuntimePool) prepareComputerDisk(context.Context, workerapi.RuntimeReconcileTarget) (*os.File, func() error, error) {
	return nil, nil, errors.New("computer runtime preparation requires Linux")
}
