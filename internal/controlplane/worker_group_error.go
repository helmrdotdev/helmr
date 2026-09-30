package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/region"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// workerGroupError maps errors of the workergroup owner, and of the region
// owner it builds on, to HTTP errors.
func workerGroupError(err error) error {
	var input workergroup.InputError
	var conflicting workergroup.ConflictError
	switch {
	case errors.As(err, &input), errors.Is(err, workergroup.ErrInvalidPlanRequest):
		return badRequest(err)
	case errors.Is(err, workergroup.ErrGroupNotFound),
		errors.Is(err, workergroup.ErrPoolNotFound),
		errors.Is(err, workergroup.ErrHostNotFound):
		return notFound(err)
	case errors.Is(err, workergroup.ErrQueuedDemand):
		return conflict(codedError{code: "queued_demand_present", message: workergroup.ErrQueuedDemand.Error()})
	case errors.As(err, &conflicting):
		return conflict(err)
	default:
		return regionError(err)
	}
}

// regionError maps errors of the region owner to HTTP errors.
func regionError(err error) error {
	var input region.InputError
	switch {
	case errors.As(err, &input):
		return badRequest(err)
	case errors.Is(err, region.ErrNotFound):
		return notFound(err)
	case errors.Is(err, region.ErrExists):
		return conflict(err)
	default:
		return err
	}
}

// writeWorkerGroupError writes a workergroup or region owner error, logging
// the failures it does not describe to the client.
func (s *Server) writeWorkerGroupError(w http.ResponseWriter, err error) {
	mapped := workerGroupError(err)
	if errorStatus(mapped) != http.StatusInternalServerError {
		writeError(w, mapped)
		return
	}
	s.log.Error("worker group request failed", "error", err)
	writeError(w, errors.New("worker group request"))
}
