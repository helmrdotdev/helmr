//go:build !linux

package worker

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/api"
)

func runPlatformAcquisitionProcess(
	context.Context,
	PlatformAcquisitionProcess,
	api.WorkerPlatformAcquisition,
) (PlatformAcquisitionProcessResult, error) {
	return PlatformAcquisitionProcessResult{}, errors.New("Platform acquisition requires Linux")
}
