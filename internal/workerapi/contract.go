package workerapi

import "fmt"

// Contract identifies the /worker/v1 wire contract that this build speaks.
// A worker sends it at enrollment and activation, and the control plane
// admits the worker only when it equals the control plane's own value.
//
// Worker and control plane form one release cohort (decision 0026), so any
// breaking change to a /worker/v1 request, response, route or error code
// advances the revision here in the same change set. The route major stays
// /worker/v1 before GA; the revision is what separates incompatible builds.
//
// Before GA the check is strict equality and there is no compatibility window.
// The self-hosted worker compatibility policy (queue item R1) may relax this
// rule after GA.
const Contract = "helmr.worker-api.v1"

// ContractMismatchCode is the error code the control plane returns, with HTTP
// status 409, when a worker's contract differs from its own.
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

// CheckContract admits only a worker that speaks this build's Contract.
func CheckContract(worker string) error {
	if worker != Contract {
		return ContractMismatchError{Worker: worker, ControlPlane: Contract}
	}
	return nil
}
