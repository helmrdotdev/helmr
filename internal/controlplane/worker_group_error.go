package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// workerGroupError maps errors of the workergroup owner, and of the region
// owner it builds on, to HTTP errors. Stale claims are checked first: a
// worker whose claims changed re-authenticates and replays whatever else
// the request would have failed with.
func workerGroupError(err error) error {
	var input workergroup.InputError
	var conflicting workergroup.ConflictError
	var policyConflict workergroup.PolicyRevisionConflict
	var scaleInConflict workergroup.ScaleInConflict
	switch {
	case errors.Is(err, workergroup.ErrCapacityUnavailable):
		return unavailable(codedError{code: "execution_capacity_unavailable", message: err.Error(), retryable: true})
	case errors.Is(err, workergroup.ErrCapacityIncompatible):
		return conflict(codedError{code: "execution_capacity_incompatible", message: err.Error()})
	case errors.Is(err, workergroup.ErrCapacityExhausted):
		return conflict(codedError{code: "execution_capacity_exhausted", message: err.Error()})
	case errors.As(err, &policyConflict), errors.As(err, &scaleInConflict):
		return conflict(err)
	case errors.Is(err, workergroup.ErrStaleClaims), errors.Is(err, workergroup.ErrUnauthenticated):
		return unauthorized(errors.New("worker authentication is required"))
	case errors.Is(err, workergroup.ErrInvalidEnrollmentToken):
		return unauthorized(err)
	case errors.Is(err, workergroup.ErrHostCustody):
		return conflict(codedError{code: "worker_resource_custody", message: workergroup.ErrHostCustody.Error()})
	case errors.Is(err, workergroup.ErrObservationConflict):
		return forbidden(err)
	case errors.As(err, &input), errors.Is(err, workergroup.ErrInvalidPlanRequest):
		return badRequest(err)
	case errors.Is(err, workergroup.ErrScaleInOperationNotFound), errors.Is(err, workergroup.ErrGroupNotFound),
		errors.Is(err, workergroup.ErrPoolNotFound),
		errors.Is(err, workergroup.ErrHostNotFound):
		return notFound(err)
	case errors.Is(err, workergroup.ErrInsufficientReadyHosts):
		return conflict(codedError{code: "insufficient_ready_hosts", message: err.Error()})
	case errors.Is(err, workergroup.ErrHostBusy):
		return conflict(codedError{code: "worker_host_busy", message: err.Error()})
	case errors.Is(err, workergroup.ErrQueuedDemand):
		return conflict(codedError{code: "queued_demand_present", message: workergroup.ErrQueuedDemand.Error()})
	case errors.As(err, &conflicting):
		return conflict(err)
	default:
		return regionError(err)
	}
}

// writeWorkerGroupError writes a workergroup owner error, including the region
// errors it surfaces, logging the failures it does not describe to the client.
func (s *Server) writeWorkerGroupError(w http.ResponseWriter, err error) {
	mapped := workerGroupError(err)
	if errorStatus(mapped) != http.StatusInternalServerError {
		writeError(w, mapped)
		return
	}
	s.log.Error("worker group request failed", "error", err)
	writeError(w, errors.New("worker group request"))
}
