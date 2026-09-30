package controlplane

import (
	"fmt"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func checkWorkerAPIVersion(version string) error {
	if version != workerapi.APIVersion {
		return conflict(codedError{
			code:    workerapi.APIVersionMismatchCode,
			message: fmt.Sprintf("worker API version %q does not match control plane API version %q; run compatible worker and control plane releases", version, workerapi.APIVersion),
		})
	}
	return nil
}
