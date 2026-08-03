//go:build !linux

package worker

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func runPlatformAcquisitionProcess(
	context.Context,
	PlatformAcquisitionProcess,
	workerapi.PlatformAcquisition,
) (PlatformAcquisitionProcessResult, error) {
	return PlatformAcquisitionProcessResult{}, errors.New("platform acquisition requires Linux")
}
