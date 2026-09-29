package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const computerCreateBodyLimit = int64(256 << 10)

type computerReference struct {
	OrgID         uuid.UUID
	ProjectID     pgtype.UUID
	EnvironmentID pgtype.UUID
	ID            string
}

func (s *Server) createComputerHTTP(w http.ResponseWriter, r *http.Request) {
	declaredID := chi.URLParam(r, "sandboxID")
	if err := definition.ValidateSandboxDeclaredID(declaredID); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_create", message: err.Error()}))
		return
	}
	var request api.CreateComputerRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		if isRequestBodyTooLarge(err) {
			writeError(w, err)
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_computer_create", message: err.Error()}))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_create", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionComputersCreate, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	result, err := s.createComputer(r.Context(), computerCreateRequest{
		OrgID:          principal.OrgID,
		ProjectID:      pgvalue.MustUUIDValue(projectID),
		EnvironmentID:  pgvalue.MustUUIDValue(environmentID),
		Declaration:    computerDeclarationSelector{Kind: computerDeclarationPromoted},
		DeclaredID:     declaredID,
		Key:            request.Key,
		Secrets:        request.Secrets,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.writeComputerCreateError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result.Snapshot)
}

func (s *Server) getComputerHTTP(w http.ResponseWriter, r *http.Request) {
	s.getComputerByReferenceHTTP(w, r)
}

func (s *Server) deleteComputerHTTP(w http.ResponseWriter, r *http.Request) {
	computerID, err := ids.Parse(chi.URLParam(r, "computerID"))
	if err != nil {
		writeError(w, badRequest(codedError{
			code:    "invalid_computer_reference",
			message: "computer ID is invalid",
		}))
		return
	}
	var request api.DeleteComputerRequest
	if err := decodeOptionalRequestJSON(r, &request); err != nil {
		if isRequestBodyTooLarge(err) {
			writeError(w, err)
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionComputersDelete, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	result, err := s.deleteComputer(r.Context(), computerDeleteRequest{
		OrgID:          principal.OrgID,
		ProjectID:      pgvalue.MustUUIDValue(projectID),
		EnvironmentID:  pgvalue.MustUUIDValue(environmentID),
		ComputerID:     computerID,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		var expired idempotency.ExpiredError
		if errors.As(err, &expired) {
			writeError(w, gone(expired))
			return
		}
		var conflictError idempotency.ConflictError
		switch {
		case errors.Is(err, errComputerNotFound):
			writeError(w, notFound(codedError{code: "computer_not_found", message: err.Error()}))
		case errors.Is(err, errComputerBusy):
			writeError(w, conflict(codedError{
				code:      "computer_busy",
				message:   err.Error(),
				retryable: true,
			}))
		case errors.As(err, &conflictError):
			writeError(w, conflict(codedError{code: "idempotency_conflict", message: err.Error()}))
		default:
			s.log.Error("delete Computer failed", "error", err)
			writeError(w, unavailable(codedError{
				code:      "computer_authority_unavailable",
				message:   errComputerAuthorityUnavailable.Error(),
				retryable: true,
			}))
		}
		return
	}
	writeJSON(w, http.StatusAccepted, api.DeleteComputerReceipt{ComputerID: result.ComputerID.String()})
}

func (s *Server) getComputerByReferenceHTTP(w http.ResponseWriter, r *http.Request) {
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionComputersRead, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	reference := computerReference{
		OrgID: principal.OrgID, ProjectID: projectID, EnvironmentID: environmentID,
		ID: chi.URLParam(r, "computerID"),
	}
	if err := ids.Validate(reference.ID); err != nil {
		writeError(w, badRequest(codedError{
			code: "invalid_computer_reference", message: "computerID must be a canonical UUIDv7",
		}))
		return
	}
	record, err := s.resolveComputerReference(r.Context(), reference)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "computer_not_found", message: "computer was not found"}))
		return
	}
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "computer_authority_unavailable",
			message:   "computer authority is unavailable",
			retryable: true,
		}))
		return
	}
	snapshot, err := s.computerSnapshot(r.Context(), s.db, record)
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "computer_authority_unavailable",
			message:   "computer authority is unavailable",
			retryable: true,
		}))
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) resolveComputerReference(
	ctx context.Context,
	reference computerReference,
) (db.GetComputerRow, error) {
	id, err := ids.Parse(reference.ID)
	if err != nil {
		return db.GetComputerRow{}, errors.New("computer ID is invalid")
	}
	return s.db.GetComputer(ctx, db.GetComputerParams{
		OrgID:         pgvalue.UUID(reference.OrgID),
		ProjectID:     reference.ProjectID,
		EnvironmentID: reference.EnvironmentID,
		ID:            pgvalue.UUID(id),
	})
}

func (s *Server) computerSnapshot(
	ctx context.Context,
	q db.Querier,
	record db.GetComputerRow,
) (api.ComputerSnapshot, error) {
	bindings, err := q.ListComputerSecrets(ctx, record.ID)
	if err != nil {
		return api.ComputerSnapshot{}, err
	}
	secrets := make([]secretbinding.Binding, 0, len(bindings))
	for _, binding := range bindings {
		item := secretbinding.Binding{Name: binding.SecretName}
		switch binding.PlacementKind {
		case "env":
			item.Env = &secretbinding.Env{Name: binding.PlacementTarget, Mode: binding.Mode, AllowedOrigins: binding.AllowedOrigins}
		case "file":
			item.File = &secretbinding.File{Path: binding.PlacementTarget}
		default:
			return api.ComputerSnapshot{}, fmt.Errorf("unsupported computer secret placement %q", binding.PlacementKind)
		}
		secrets = append(secrets, item)
	}
	status, err := computerPublicStatus(record.Status)
	if err != nil {
		return api.ComputerSnapshot{}, err
	}
	var key *string
	if record.Key.Valid {
		value := record.Key.String
		key = &value
	}
	return api.ComputerSnapshot{
		ID:             pgvalue.UUIDString(record.ID),
		Key:            key,
		SandboxID:      record.SandboxDeclaredID.String,
		DeploymentID:   pgvalue.UUIDString(record.CreationDeploymentID),
		Status:         status,
		Error:          record.ResidencyError,
		Residency:      api.ComputerResidency(record.Residency),
		Secrets:        secrets,
		LastActivityAt: pgvalue.Time(record.LastActivityAt),
		CreatedAt:      pgvalue.Time(record.CreatedAt),
		UpdatedAt:      pgvalue.Time(record.UpdatedAt),
	}, nil
}

func computerPublicStatus(state string) (api.ComputerStatus, error) {
	switch state {
	case db.ComputerStatusActive:
		return api.ComputerStatusAvailable, nil
	case db.ComputerStatusRecoveryRequired:
		return api.ComputerStatusAvailable, nil
	case db.ComputerStatusDeleted:
		return api.ComputerStatusDeleted, nil
	case db.ComputerStatusDeleting:
		return api.ComputerStatusDeleting, nil
	default:
		return "", fmt.Errorf("computer state %q has no public projection", state)
	}
}

func (s *Server) writeComputerCreateError(w http.ResponseWriter, err error) {
	var expired idempotency.ExpiredError
	if errors.As(err, &expired) {
		writeError(w, gone(expired))
		return
	}
	var conflictError idempotency.ConflictError
	var keyConflict ComputerKeyConflictError
	switch {
	case errors.Is(err, errComputerCreateInvalid):
		writeError(w, badRequest(codedError{code: "invalid_computer_create", message: err.Error()}))
	case errors.Is(err, errComputerNotDeployed):
		writeError(w, notFound(codedError{code: "computer_not_deployed", message: err.Error()}))
	case errors.Is(err, errComputerSecretUnavailable):
		writeError(w, conflict(codedError{code: "secret_unavailable", message: err.Error()}))
	case errors.As(err, &conflictError):
		writeError(w, conflict(codedError{code: "idempotency_conflict", message: err.Error()}))
	case errors.As(err, &keyConflict):
		writeError(w, conflict(codedError{code: "computer_key_conflict", message: err.Error()}))
	default:
		s.log.Error("create Computer failed", "error", err)
		writeError(w, unavailable(codedError{
			code:      "computer_authority_unavailable",
			message:   errComputerAuthorityUnavailable.Error(),
			retryable: true,
		}))
	}
}
