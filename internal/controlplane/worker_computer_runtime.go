package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerCreateComputer(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CreateComputerRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid computer create JSON: %w", err))
		return
	}
	if err := validateWorkerComputerCorrelation(request.CorrelationID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := definition.ValidateSandboxDeclaredID(request.SandboxDeclaredID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	fence, err := workerSourceReceipt(workerFromContext(r.Context()), request.Lease).Fence()
	if err != nil {
		s.writeWorkerComputerSourceError(w, "create", request.Lease.ID, err)
		return
	}
	result, err := run.CreateComputer(r.Context(), s.tx, s.computers, fence, run.ComputerCreation{
		DeclaredID: request.SandboxDeclaredID,
		Key:        request.Key, Secrets: request.Secrets, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		if isStaleWorkerRunSource(err) {
			s.writeWorkerComputerSourceError(w, "create", request.Lease.ID, err)
			return
		}
		if failure, ok := workerComputerFailure(err, computerCreateOperation); ok {
			writeJSON(w, http.StatusOK, workerapi.CreateComputerResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		s.log.Error("create run-sourced Computer", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("create run-sourced computer"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.CreateComputerResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &workerapi.CreateComputerResult{ComputerID: result.ComputerID.String()},
	})
}

func (s *Server) workerRetrieveComputer(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RetrieveComputerRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid computer retrieve JSON: %w", err))
		return
	}
	computerID, err := parseWorkerComputerRequest(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	fence, err := workerSourceReceipt(workerFromContext(r.Context()), request.Lease).Fence()
	if err != nil {
		s.writeWorkerComputerSourceError(w, "retrieve", request.Lease.ID, err)
		return
	}
	snapshot, err := run.ReadComputer(r.Context(), s.tx, fence, computerID)
	if s.writeWorkerComputerReadResult(w, request.CorrelationID, request.Lease.ID, "retrieve", err) {
		return
	}
	completed := apiComputerSnapshot(snapshot)
	writeJSON(w, http.StatusOK, workerapi.RetrieveComputerResponse{
		CorrelationID: request.CorrelationID, Completed: &completed,
	})
}

func (s *Server) workerListComputerMembers(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerMembersRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid computer members JSON: %w", err))
		return
	}
	computerID, err := parseWorkerComputerRequest(request.RetrieveComputerRequest)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	fence, err := workerSourceReceipt(workerFromContext(r.Context()), request.Lease).Fence()
	if err != nil {
		s.writeWorkerComputerSourceError(w, "members", request.Lease.ID, err)
		return
	}
	page, err := run.ListComputerMembers(r.Context(), s.tx, fence, computerID, computer.MembersQuery{
		Cursor: request.Cursor, Limit: request.Limit,
	})
	var input computer.InputError
	if errors.As(err, &input) {
		writeJSON(w, http.StatusOK, workerapi.ComputerMembersResponse{
			CorrelationID: request.CorrelationID,
			Failed:        &workerapi.RuntimeOperationFailure{Code: "invalid_computer_reference", Message: err.Error()},
		})
		return
	}
	if s.writeWorkerComputerReadResult(w, request.CorrelationID, request.Lease.ID, "members", err) {
		return
	}
	completed := apiComputerMembers(page)
	writeJSON(w, http.StatusOK, workerapi.ComputerMembersResponse{CorrelationID: request.CorrelationID, Completed: &completed})
}

func (s *Server) workerDeleteComputer(w http.ResponseWriter, r *http.Request) {
	var request workerapi.DeleteComputerRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid computer delete JSON: %w", err))
		return
	}
	computerID, err := parseWorkerComputerRequest(request.RetrieveComputerRequest)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	fence, err := workerSourceReceipt(workerFromContext(r.Context()), request.Lease).Fence()
	if err != nil {
		s.writeWorkerComputerSourceError(w, "delete", request.Lease.ID, err)
		return
	}
	result, err := run.DeleteComputer(r.Context(), s.tx, fence, computerID, idempotencyKey)
	if err != nil {
		if isStaleWorkerRunSource(err) {
			s.writeWorkerComputerSourceError(w, "delete", request.Lease.ID, err)
			return
		}
		if failure, ok := workerComputerFailure(err, computerDeleteOperation); ok {
			writeJSON(w, http.StatusOK, workerapi.DeleteComputerResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		s.log.Error("delete run-sourced Computer", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("delete run-sourced computer"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.DeleteComputerResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &api.DeleteComputerReceipt{ComputerID: result.ComputerID.String()},
	})
}

func isStaleWorkerRunSource(err error) bool {
	return errors.Is(err, run.ErrStaleSource) || errors.Is(err, workergroup.ErrStaleClaims)
}

// parseWorkerComputerRequest validates a run-sourced Computer request's
// correlation and returns the addressed Computer.
func parseWorkerComputerRequest(request workerapi.RetrieveComputerRequest) (uuid.UUID, error) {
	if err := validateWorkerComputerCorrelation(request.CorrelationID); err != nil {
		return uuid.UUID{}, err
	}
	computerID, err := ids.Parse(request.Computer.ComputerID)
	if err != nil {
		return uuid.UUID{}, errors.New("computer ID is invalid")
	}
	return computerID, nil
}

func validateWorkerComputerCorrelation(value string) error {
	if err := ids.Validate(value); err != nil {
		return errors.New("computer runtime correlation ID is invalid")
	}
	return nil
}

func (s *Server) writeWorkerComputerReadResult(
	w http.ResponseWriter,
	correlationID string,
	runID string,
	operation string,
	err error,
) bool {
	if err == nil {
		return false
	}
	if failure, ok := workerComputerFailure(err, computerReadOperation); ok {
		writeJSON(w, http.StatusOK, workerapi.RetrieveComputerResponse{
			CorrelationID: correlationID, Failed: &failure,
		})
		return true
	}
	s.writeWorkerComputerSourceError(w, operation, runID, err)
	return true
}

func (s *Server) writeWorkerComputerSourceError(
	w http.ResponseWriter,
	operation string,
	runID string,
	err error,
) {
	if writeStaleWorkerClaims(w, err) {
		return
	}
	if errors.Is(err, run.ErrStaleSource) {
		writeError(w, conflict(run.ErrStaleSource))
		return
	}
	s.log.Error("authorize worker Computer operation source",
		"operation", operation, "run_id", runID, "error", err)
	writeError(w, errors.New("authorize worker computer operation source"))
}

// workerProtectedEnv is the protected environment a guest receives for a
// Computer, or nil when it has no protected binding.
func workerProtectedEnv(ctx context.Context, q db.Querier, environmentID, computerID pgtype.UUID) (*workerapi.ProtectedEnv, error) {
	protected, err := computer.ReadProtectedEnv(ctx, q, environmentID, computerID)
	if err != nil || protected == nil {
		return nil, err
	}
	return &workerapi.ProtectedEnv{Env: protected.Env, CA: protected.CA}, nil
}
