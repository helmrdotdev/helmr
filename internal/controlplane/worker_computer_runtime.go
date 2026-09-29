package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
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
	worker := workerFromContext(r.Context())
	source, err := s.workerRunSource(r.Context(), worker, request.Lease)
	if err != nil {
		s.writeWorkerComputerSourceError(w, "create", request.Lease.ID, err)
		return
	}
	result, err := s.createComputer(r.Context(), computerCreateRequest{
		SourceComputerID: source.ComputerID,
		OrgID:            pgvalue.MustUUIDValue(source.OrgID),
		ProjectID:        pgvalue.MustUUIDValue(source.ProjectID),
		EnvironmentID:    pgvalue.MustUUIDValue(source.EnvironmentID),
		Declaration: computerDeclarationSelector{
			Kind:  computerDeclarationRunPinned,
			RunID: pgvalue.MustUUIDValue(source.RunID),
		},
		DeclaredID: request.SandboxDeclaredID,
		Key:        request.Key, Secrets: request.Secrets, IdempotencyKey: idempotencyKey,
		Authorize: func(ctx context.Context, tx pgx.Tx) error {
			q := db.New(tx)
			// Secret rows precede mutable source runtime authority, including replays.
			if err := authorizeComputerSecretCreate(ctx, q, source.ComputerID, source.EnvironmentID, request.Secrets); err != nil {
				return err
			}
			_, err := authorizeWorkerRunSource(ctx, tx, worker, request.Lease)
			return err
		},
	})
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) || errors.Is(err, errStaleWorkerClaims) {
			s.writeWorkerComputerSourceError(w, "create", request.Lease.ID, err)
			return
		}
		if failure, ok := workerComputerCreateFailure(err); ok {
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
	if err := validateWorkerComputerRequest(request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	var snapshot api.ComputerSnapshot
	err := s.inTx(r.Context(), func(work *txWork) error {
		source, err := authorizeWorkerRunSource(r.Context(), work.tx, worker, request.Lease)
		if err != nil {
			return err
		}
		record, err := resolveWorkerComputer(r.Context(), work.q, source, request.Computer)
		if err != nil {
			return err
		}
		snapshot, err = s.computerSnapshot(r.Context(), work.q, record)
		return err
	})
	if s.writeWorkerComputerReadResult(w, request.CorrelationID, request.Lease.ID, "retrieve", err) {
		return
	}
	writeJSON(w, http.StatusOK, workerapi.RetrieveComputerResponse{
		CorrelationID: request.CorrelationID, Completed: &snapshot,
	})
}

func (s *Server) workerListComputerMembers(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerMembersRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid computer members JSON: %w", err))
		return
	}
	if err := validateWorkerComputerRequest(request.RetrieveComputerRequest); err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	response := workerapi.ComputerMembersResponse{CorrelationID: request.CorrelationID}
	err := s.inTx(r.Context(), func(work *txWork) error {
		source, err := authorizeWorkerRunSource(r.Context(), work.tx, worker, request.Lease)
		if err != nil {
			return err
		}
		record, err := resolveWorkerComputer(r.Context(), work.q, source, request.Computer)
		if err != nil {
			return err
		}
		params, err := computerMembersParams(record.EnvironmentID, record.ID, request.ComputerMembersQuery)
		if err != nil {
			response.Failed = &workerapi.RuntimeOperationFailure{Code: "invalid_computer_reference", Message: err.Error()}
			return nil
		}
		snapshot, err := listComputerMembers(r.Context(), work.q, params)
		if err == nil {
			response.Completed = &snapshot
		}
		return err
	})
	if s.writeWorkerComputerReadResult(w, request.CorrelationID, request.Lease.ID, "members", err) {
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerDeleteComputer(w http.ResponseWriter, r *http.Request) {
	var request workerapi.DeleteComputerRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid computer delete JSON: %w", err))
		return
	}
	if err := validateWorkerComputerRequest(request.RetrieveComputerRequest); err != nil {
		writeError(w, badRequest(err))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	source, err := s.workerRunSource(r.Context(), worker, request.Lease)
	if err != nil {
		if failure, ok := workerComputerReferenceFailure(err); ok {
			writeJSON(w, http.StatusOK, workerapi.DeleteComputerResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		s.writeWorkerComputerSourceError(w, "delete", request.Lease.ID, err)
		return
	}
	computerID, err := ids.Parse(request.Computer.ComputerID)
	if err != nil {
		writeError(w, badRequest(errors.New("computer ID is invalid")))
		return
	}
	result, err := s.deleteComputer(r.Context(), computerDeleteRequest{
		OrgID: pgvalue.MustUUIDValue(source.OrgID), ProjectID: pgvalue.MustUUIDValue(source.ProjectID),
		EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID), ComputerID: computerID,
		IdempotencyKey: idempotencyKey,
		Authorize: func(ctx context.Context, tx pgx.Tx) error {
			_, err := authorizeWorkerRunSourceForComputer(ctx, tx, worker, request.Lease, pgvalue.UUID(computerID))
			return err
		},
	})
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) || errors.Is(err, errStaleWorkerClaims) {
			s.writeWorkerComputerSourceError(w, "delete", request.Lease.ID, err)
			return
		}
		if failure, ok := workerComputerDeleteFailure(err); ok {
			writeJSON(w, http.StatusOK, workerapi.DeleteComputerResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		writeError(w, errors.New("delete run-sourced computer"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.DeleteComputerResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &api.DeleteComputerReceipt{ComputerID: result.ComputerID.String()},
	})
}

func resolveWorkerComputer(
	ctx context.Context,
	q db.Querier,
	source workerRunSourceAuthority,
	address workerapi.ComputerAddress,
) (db.GetComputerRow, error) {
	id, err := ids.Parse(address.ComputerID)
	if err != nil {
		return db.GetComputerRow{}, err
	}
	return q.GetComputer(ctx, db.GetComputerParams{
		OrgID: source.OrgID, ProjectID: source.ProjectID, EnvironmentID: source.EnvironmentID,
		ID: pgvalue.UUID(id),
	})
}

func validateWorkerComputerRequest(request workerapi.RetrieveComputerRequest) error {
	if err := validateWorkerComputerCorrelation(request.CorrelationID); err != nil {
		return err
	}
	if err := ids.Validate(request.Computer.ComputerID); err != nil {
		return errors.New("computer ID is invalid")
	}
	return nil
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
	if failure, ok := workerComputerReferenceFailure(err); ok {
		writeJSON(w, http.StatusOK, workerapi.RetrieveComputerResponse{
			CorrelationID: correlationID, Failed: &failure,
		})
		return true
	}
	s.writeWorkerComputerSourceError(w, operation, runID, err)
	return true
}

func workerComputerReferenceFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return workerapi.RuntimeOperationFailure{Code: "computer_not_found", Message: "Computer was not found"}, true
	case errors.Is(err, errStaleWorkerRunSource):
		return workerapi.RuntimeOperationFailure{}, false
	default:
		return workerapi.RuntimeOperationFailure{}, false
	}
}

func workerComputerCreateFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	var keyConflict ComputerKeyConflictError
	var expired idempotency.ExpiredError
	if errors.As(err, &expired) {
		return workerapi.RuntimeOperationFailure{Code: expired.ErrorCode(), Message: expired.Error()}, true
	}
	var idempotencyConflict idempotency.ConflictError
	switch {
	case errors.Is(err, errComputerCreateInvalid):
		return workerapi.RuntimeOperationFailure{Code: "invalid_computer_create", Message: err.Error()}, true
	case errors.Is(err, errComputerNotDeployed):
		return workerapi.RuntimeOperationFailure{Code: "computer_not_deployed", Message: err.Error()}, true
	case errors.Is(err, errComputerSecretUnavailable):
		return workerapi.RuntimeOperationFailure{Code: "secret_unavailable", Message: err.Error()}, true
	case errors.As(err, &keyConflict):
		return workerapi.RuntimeOperationFailure{Code: "computer_key_conflict", Message: err.Error()}, true
	case errors.As(err, &idempotencyConflict):
		return workerapi.RuntimeOperationFailure{Code: "idempotency_conflict", Message: err.Error()}, true
	default:
		return workerapi.RuntimeOperationFailure{}, false
	}
}

func workerComputerDeleteFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	var expired idempotency.ExpiredError
	if errors.As(err, &expired) {
		return workerapi.RuntimeOperationFailure{Code: expired.ErrorCode(), Message: expired.Error()}, true
	}
	var idempotencyConflict idempotency.ConflictError
	switch {
	case errors.Is(err, errComputerNotFound):
		return workerapi.RuntimeOperationFailure{Code: "computer_not_found", Message: err.Error()}, true
	case errors.Is(err, errComputerBusy):
		return workerapi.RuntimeOperationFailure{Code: "computer_busy", Message: err.Error(), Retryable: true}, true
	case errors.As(err, &idempotencyConflict):
		return workerapi.RuntimeOperationFailure{Code: "idempotency_conflict", Message: err.Error()}, true
	default:
		return workerapi.RuntimeOperationFailure{}, false
	}
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
	if errors.Is(err, errStaleWorkerRunSource) {
		writeError(w, conflict(errStaleWorkerRunSource))
		return
	}
	s.log.Error("authorize worker Computer operation source",
		"operation", operation, "run_id", runID, "error", err)
	writeError(w, errors.New("authorize worker computer operation source"))
}
