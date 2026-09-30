package controlplane

import (
	"fmt"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func checkWorkerContract(contract string) error {
	if contract != workerapi.Contract {
		return conflict(codedError{
			code:    workerapi.ContractMismatchCode,
			message: fmt.Sprintf("worker API contract %q does not match control plane contract %q; run compatible worker and control plane releases", contract, workerapi.Contract),
		})
	}
	return nil
}
