package workerapi

import "fmt"

// Contract identifies the /worker/v1 wire contract that this build speaks:
// the route major followed by a revision. The worker client sends it in
// ContractHeader on every /worker/v1 request, and the control plane admits
// only requests whose value equals its own.
//
// Worker and control plane form one release cohort (decision 0026). Before
// GA, any breaking change to a /worker/v1 request, response, header, route or
// error code bumps the revision in the same change set; the route major
// stays v1. The check is strict equality with no compatibility window; the
// self-hosted worker compatibility policy (queue item R1) may relax it after
// GA.
const Contract = "helmr.worker-api.v1.r1"

// ContractHeader carries Contract on every /worker/v1 request.
const ContractHeader = "Helmr-Worker-Contract"

// ContractMismatchCode is the error code the control plane returns, with HTTP
// status 409, for a /worker/v1 request whose contract differs from its own or
// is missing.
const ContractMismatchCode = "worker_contract_mismatch"

// ContractMismatch detail keys name both contracts in the error response.
const (
	ContractMismatchWorkerDetail       = "worker_contract"
	ContractMismatchControlPlaneDetail = "control_plane_contract"
)

// ContractMismatchError reports a worker and control plane built for
// different worker API contracts.
type ContractMismatchError struct {
	Worker       string
	ControlPlane string
}

func (e ContractMismatchError) Error() string {
	return fmt.Sprintf(
		"worker API contract %q does not match control plane contract %q; run the worker and control plane from the same Helmr release",
		e.Worker, e.ControlPlane,
	)
}

// ContractMismatch reports whether the ContractHeader values of a request
// come from a worker on another contract. Only exactly one value equal to
// Contract matches; a missing or repeated header is a mismatch.
func ContractMismatch(values []string) (ContractMismatchError, bool) {
	if len(values) == 1 && values[0] == Contract {
		return ContractMismatchError{}, false
	}
	worker := ""
	if len(values) == 1 {
		worker = values[0]
	} else if len(values) > 1 {
		worker = fmt.Sprint(values)
	}
	return ContractMismatchError{Worker: worker, ControlPlane: Contract}, true
}
