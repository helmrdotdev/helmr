package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultWorkspaceListLimit      = int32(100)
	maxWorkspaceListLimit          = int32(200)
	defaultWorkspaceIdempotencyTTL = 24 * time.Hour
	workspaceCreateOperationKind   = db.WorkspaceOperationIdempotencyKindWorkspaceCreate
	workspaceStopOperationKind     = db.WorkspaceOperationIdempotencyKindWorkspaceStop
)

var errWorkspaceOperationPending = codedError{code: "idempotency_pending", message: "workspace_operation_pending"}

type workspaceOperationIdempotencyStore interface {
	EnsureWorkspaceOperationIdempotency(context.Context, db.EnsureWorkspaceOperationIdempotencyParams) (db.EnsureWorkspaceOperationIdempotencyRow, error)
	GetWorkspaceOperationIdempotency(context.Context, db.GetWorkspaceOperationIdempotencyParams) (db.WorkspaceOperationIdempotency, error)
	GetWorkspaceScopedOperationIdempotency(context.Context, db.GetWorkspaceScopedOperationIdempotencyParams) (db.WorkspaceOperationIdempotency, error)
}

func ensureWorkspaceOperationIdempotency(ctx context.Context, store workspaceOperationIdempotencyStore, params db.EnsureWorkspaceOperationIdempotencyParams) (db.EnsureWorkspaceOperationIdempotencyRow, error) {
	row, err := store.EnsureWorkspaceOperationIdempotency(ctx, params)
	if !isNoRows(err) {
		return row, err
	}
	var existing db.WorkspaceOperationIdempotency
	if params.WorkspaceID.Valid {
		existing, err = store.GetWorkspaceScopedOperationIdempotency(ctx, db.GetWorkspaceScopedOperationIdempotencyParams{
			OrgID:          params.OrgID,
			ProjectID:      params.ProjectID,
			EnvironmentID:  params.EnvironmentID,
			WorkspaceID:    params.WorkspaceID,
			OperationKind:  params.OperationKind,
			IdempotencyKey: params.IdempotencyKey,
		})
	} else {
		existing, err = store.GetWorkspaceOperationIdempotency(ctx, db.GetWorkspaceOperationIdempotencyParams{
			OrgID:          params.OrgID,
			ProjectID:      params.ProjectID,
			EnvironmentID:  params.EnvironmentID,
			OperationKind:  params.OperationKind,
			IdempotencyKey: params.IdempotencyKey,
		})
	}
	if err != nil {
		return db.EnsureWorkspaceOperationIdempotencyRow{}, err
	}
	return db.EnsureWorkspaceOperationIdempotencyRow{
		ID:                   existing.ID,
		OrgID:                existing.OrgID,
		ProjectID:            existing.ProjectID,
		EnvironmentID:        existing.EnvironmentID,
		WorkspaceID:          existing.WorkspaceID,
		OperationKind:        existing.OperationKind,
		IdempotencyKey:       existing.IdempotencyKey,
		RequestFingerprint:   existing.RequestFingerprint,
		ResponseResourceType: existing.ResponseResourceType,
		ResponseResourceID:   existing.ResponseResourceID,
		ResponseBody:         existing.ResponseBody,
		ExpiresAt:            existing.ExpiresAt,
		CreatedAt:            existing.CreatedAt,
		LastUsedAt:           existing.LastUsedAt,
		Inserted:             false,
	}, nil
}

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("workspace storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	var request api.WorkspaceCreateRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace request JSON: %w", err)))
		return
	}
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionWorkspaceLifecycleManage, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	workspace, cached, err := s.createWorkspaceForRequest(r.Context(), actor, projectID, environmentID, request)
	if err != nil {
		s.writeWorkspaceError(w, "create workspace", err)
		return
	}
	writeJSON(w, http.StatusCreated, api.WorkspaceEnvelope{Workspace: workspaceResponse(workspace), IsCached: cached})
}

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("workspace storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actorHasAnyPermission(actor, scope, workspaceReadPermissions()...) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	limit, err := parseWorkspaceListLimit(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	state, err := optionalWorkspaceState(r.URL.Query().Get("state"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	rows, err := s.db.ListWorkspaces(r.Context(), db.ListWorkspacesParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		State:         state,
		ExternalID:    optionalText(r.URL.Query().Get("external_id")),
		Tag:           optionalText(r.URL.Query().Get("tag")),
		LimitCount:    limit,
	})
	if err != nil {
		s.log.Error("list workspaces failed", "error", err)
		writeError(w, errors.New("list workspaces"))
		return
	}
	out := make([]api.WorkspaceResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, workspaceResponse(row))
	}
	writeJSON(w, http.StatusOK, api.ListWorkspacesResponse{Workspaces: out})
}

func (s *Server) getWorkspace(w http.ResponseWriter, r *http.Request) {
	row, ok := s.loadWorkspaceForRequest(w, r, workspaceReadPermissions()...)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspaceEnvelope{Workspace: workspaceResponse(row)})
}

func (s *Server) patchWorkspace(w http.ResponseWriter, r *http.Request) {
	current, ok := s.loadWorkspaceForRequest(w, r, auth.PermissionWorkspaceLifecycleManage)
	if !ok {
		return
	}
	var request api.WorkspacePatchRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace patch request JSON: %w", err)))
		return
	}
	var metadata []byte
	if request.Metadata != nil {
		normalized, err := normalizedJSONObject(request.Metadata, "metadata")
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		metadata = normalized
	}
	var tags []string
	if request.Tags != nil {
		normalizedTags, err := normalizedRunTags(request.Tags)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		tags = normalizedTags
	}
	row, err := s.db.PatchWorkspace(r.Context(), db.PatchWorkspaceParams{
		OrgID:         current.OrgID,
		ProjectID:     current.ProjectID,
		EnvironmentID: current.EnvironmentID,
		ID:            current.ID,
		Metadata:      metadata,
		Tags:          tags,
	})
	if err != nil {
		s.writeWorkspaceError(w, "patch workspace", err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspaceEnvelope{Workspace: workspaceResponse(row)})
}

func (s *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	current, ok := s.loadWorkspaceForRequest(w, r, auth.PermissionWorkspaceLifecycleManage)
	if !ok {
		return
	}
	row, err := s.db.ArchiveWorkspace(r.Context(), db.ArchiveWorkspaceParams{
		OrgID:         current.OrgID,
		ProjectID:     current.ProjectID,
		EnvironmentID: current.EnvironmentID,
		ID:            current.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("workspace has an active workspace mount")))
		return
	}
	if err != nil {
		s.writeWorkspaceError(w, "archive workspace", err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspaceEnvelope{Workspace: workspaceResponse(row)})
}

func (s *Server) connectWorkspace(w http.ResponseWriter, r *http.Request) {
	s.requestWorkspaceMount(w, r)
}

func (s *Server) createWorkspaceForRequest(ctx context.Context, actor auth.Actor, projectID pgtype.UUID, environmentID pgtype.UUID, request api.WorkspaceCreateRequest) (db.Workspace, bool, error) {
	sandboxID := strings.TrimSpace(request.SandboxID)
	if sandboxID == "" {
		return db.Workspace{}, false, errors.New("sandbox_id is required")
	}
	deploymentID, err := parseOptionalWorkspaceUUID("deployment_id", request.DeploymentID)
	if err != nil {
		return db.Workspace{}, false, err
	}
	metadata, err := normalizedJSONObject(request.Metadata, "metadata")
	if err != nil {
		return db.Workspace{}, false, err
	}
	tags, err := normalizedRunTags(request.Tags)
	if err != nil {
		return db.Workspace{}, false, err
	}
	fingerprint, err := workspaceCreateFingerprint(request, metadata, tags)
	if err != nil {
		return db.Workspace{}, false, err
	}
	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	var idempotencyTTL time.Duration
	if idempotencyKey != "" {
		idempotencyTTL, err = workspaceIdempotencyTTL(request.IdempotencyKeyTTL)
		if err != nil {
			return db.Workspace{}, false, err
		}
		cached, err := s.db.GetWorkspaceOperationIdempotency(ctx, db.GetWorkspaceOperationIdempotencyParams{
			OrgID:          pgvalue.UUID(actor.OrgID),
			ProjectID:      projectID,
			EnvironmentID:  environmentID,
			OperationKind:  workspaceCreateOperationKind,
			IdempotencyKey: idempotencyKey,
		})
		if err == nil {
			if cached.RequestFingerprint != fingerprint {
				return db.Workspace{}, false, codedError{code: "idempotency_fingerprint_mismatch", message: "idempotency_key was already used with different workspace create parameters"}
			}
			if !cached.ResponseResourceID.Valid {
				return db.Workspace{}, false, errWorkspaceOperationPending
			}
			row, err := s.db.GetWorkspace(ctx, db.GetWorkspaceParams{
				OrgID:         pgvalue.UUID(actor.OrgID),
				ProjectID:     projectID,
				EnvironmentID: environmentID,
				ID:            cached.ResponseResourceID,
			})
			return row, true, err
		}
		if !isNoRows(err) {
			return db.Workspace{}, false, err
		}
	}
	deploymentSandbox, err := s.db.ResolveDeploymentSandboxForWorkspaceCreate(ctx, db.ResolveDeploymentSandboxForWorkspaceCreateParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		SandboxID:     sandboxID,
		DeploymentID:  deploymentID,
	})
	if err != nil {
		if isNoRows(err) {
			return db.Workspace{}, false, errSandboxNotDeployed
		}
		return db.Workspace{}, false, err
	}
	if s.cas == nil {
		return db.Workspace{}, false, errors.New("workspace artifact CAS is not configured")
	}
	var row db.Workspace
	replayed := false
	err = s.inTx(ctx, func(work *txWork) error {
		workspaceStore := work.q
		if idempotencyKey != "" {
			idempotency, err := ensureWorkspaceOperationIdempotency(ctx, workspaceStore, db.EnsureWorkspaceOperationIdempotencyParams{
				ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:                pgvalue.UUID(actor.OrgID),
				ProjectID:            projectID,
				EnvironmentID:        environmentID,
				WorkspaceID:          pgtype.UUID{},
				OperationKind:        workspaceCreateOperationKind,
				IdempotencyKey:       idempotencyKey,
				RequestFingerprint:   fingerprint,
				ResponseResourceType: "",
				ResponseResourceID:   pgtype.UUID{},
				ResponseBody:         []byte(`{}`),
				ExpiresAt:            pgvalue.Timestamptz(time.Now().Add(idempotencyTTL)),
			})
			if err != nil {
				return err
			}
			if !idempotency.Inserted {
				if idempotency.RequestFingerprint != fingerprint {
					return codedError{code: "idempotency_fingerprint_mismatch", message: "idempotency_key was already used with different workspace create parameters"}
				}
				if !idempotency.ResponseResourceID.Valid {
					return errWorkspaceOperationPending
				}
				existing, getWorkspaceErr := workspaceStore.GetWorkspace(ctx, db.GetWorkspaceParams{
					OrgID:         pgvalue.UUID(actor.OrgID),
					ProjectID:     projectID,
					EnvironmentID: environmentID,
					ID:            idempotency.ResponseResourceID,
				})
				row = existing
				replayed = true
				return getWorkspaceErr
			}
		}
		workspaceArtifact, emptyArtifact, err := s.createInitialWorkspaceArtifact(ctx, workspaceStore, actor.OrgID, projectID, environmentID)
		if err != nil {
			return err
		}
		created, err := workspaceStore.CreateWorkspaceFromSandbox(ctx, db.CreateWorkspaceFromSandboxParams{
			ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:                     pgvalue.UUID(actor.OrgID),
			ProjectID:                 projectID,
			EnvironmentID:             environmentID,
			DeploymentSandboxID:       deploymentSandbox.ID,
			ExternalID:                strings.TrimSpace(request.ExternalID),
			Metadata:                  metadata,
			Tags:                      tags,
			RetentionPolicy:           []byte(`{}`),
			InitialVersionID:          pgvalue.UUID(uuid.Must(uuid.NewV7())),
			InitialArtifactID:         workspaceArtifact.ID,
			InitialArtifactEncoding:   emptyArtifact.Encoding,
			InitialArtifactEntryCount: int32(emptyArtifact.EntryCount),
			InitialContentDigest:      workspaceArtifact.Digest,
			InitialSizeBytes:          workspaceArtifact.SizeBytes,
		})
		if err != nil {
			return err
		}
		row = workspaceFromCreateWorkspaceFromSandbox(created)
		if idempotencyKey != "" {
			_, err = workspaceStore.CompleteWorkspaceOperationIdempotency(ctx, db.CompleteWorkspaceOperationIdempotencyParams{
				OrgID:                pgvalue.UUID(actor.OrgID),
				ProjectID:            projectID,
				EnvironmentID:        environmentID,
				OperationKind:        workspaceCreateOperationKind,
				IdempotencyKey:       idempotencyKey,
				RequestFingerprint:   fingerprint,
				ResponseResourceType: "workspace",
				ResponseResourceID:   row.ID,
				ResponseBody:         []byte(`{}`),
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return db.Workspace{}, false, err
	}
	return row, replayed, nil
}

func workspaceFromCreateWorkspaceFromSandbox(row db.CreateWorkspaceFromSandboxRow) db.Workspace {
	return db.Workspace(row)
}

func (s *Server) createInitialWorkspaceArtifact(ctx context.Context, store db.Querier, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID) (db.Artifact, workspace.WorkspaceArtifact, error) {
	if s.cas == nil {
		return db.Artifact{}, workspace.WorkspaceArtifact{}, errors.New("workspace artifact CAS is not configured")
	}
	emptyArtifact, cleanupEmptyArtifact, err := workspace.CreateEmptyWorkspaceArtifact(os.TempDir())
	if err != nil {
		return db.Artifact{}, workspace.WorkspaceArtifact{}, err
	}
	defer cleanupEmptyArtifact()
	artifactFile, err := os.Open(emptyArtifact.Path)
	if err != nil {
		return db.Artifact{}, workspace.WorkspaceArtifact{}, fmt.Errorf("open initial workspace artifact: %w", err)
	}
	object, putErr := s.cas.Put(ctx, emptyArtifact.MediaType, artifactFile)
	closeErr := artifactFile.Close()
	if putErr != nil {
		return db.Artifact{}, workspace.WorkspaceArtifact{}, fmt.Errorf("store initial workspace artifact: %w", putErr)
	}
	if closeErr != nil {
		return db.Artifact{}, workspace.WorkspaceArtifact{}, fmt.Errorf("close initial workspace artifact: %w", closeErr)
	}
	if object.Digest != emptyArtifact.Digest || object.SizeBytes != emptyArtifact.SizeBytes || object.MediaType != emptyArtifact.MediaType {
		return db.Artifact{}, workspace.WorkspaceArtifact{}, errors.New("initial workspace artifact CAS metadata mismatch")
	}
	if _, err := store.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		OrgID:     pgvalue.UUID(orgID),
		CellID:    s.cellID,
		Digest:    object.Digest,
		SizeBytes: object.SizeBytes,
		MediaType: object.MediaType,
	}); err != nil {
		return db.Artifact{}, workspace.WorkspaceArtifact{}, err
	}
	workspaceArtifact, err := store.CreateArtifact(ctx, db.CreateArtifactParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(orgID),
		CellID:        s.cellID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Digest:        object.Digest,
		Kind:          db.ArtifactKindWorkspaceVersion,
		SizeBytes:     object.SizeBytes,
		MediaType:     object.MediaType,
	})
	if err != nil {
		return db.Artifact{}, workspace.WorkspaceArtifact{}, err
	}
	return workspaceArtifact, emptyArtifact, nil
}

func (s *Server) loadWorkspaceForRequest(w http.ResponseWriter, r *http.Request, permissions ...auth.Permission) (db.Workspace, bool) {
	workspaceID, err := parseUUIDParam(r, "workspaceID")
	if err != nil {
		writeError(w, badRequest(err))
		return db.Workspace{}, false
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
	if err != nil {
		writeError(w, badRequest(err))
		return db.Workspace{}, false
	}
	if !actorHasAnyPermission(actor, scope, permissions...) {
		writeError(w, forbidden(errors.New("permission is required")))
		return db.Workspace{}, false
	}
	row, err := s.db.GetWorkspace(r.Context(), db.GetWorkspaceParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ID:            pgvalue.UUID(workspaceID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("workspace not found")))
		return db.Workspace{}, false
	}
	if err != nil {
		s.log.Error("load workspace failed", "workspace_id", workspaceID.String(), "error", err)
		writeError(w, errors.New("load workspace"))
		return db.Workspace{}, false
	}
	return row, true
}

func workspaceResponse(row db.Workspace) api.WorkspaceResponse {
	response := api.WorkspaceResponse{
		ID:                  pgvalue.MustUUIDValue(row.ID).String(),
		ProjectID:           pgvalue.MustUUIDValue(row.ProjectID).String(),
		EnvironmentID:       pgvalue.MustUUIDValue(row.EnvironmentID).String(),
		DeploymentSandboxID: pgvalue.MustUUIDValue(row.DeploymentSandboxID).String(),
		SandboxID:           row.SandboxID,
		SandboxFingerprint:  row.SandboxFingerprint,
		ExternalID:          row.ExternalID,
		State:               string(row.State),
		DesiredState:        string(row.DesiredState),
		DirtyState:          string(row.DirtyState),
		Metadata:            json.RawMessage(row.Metadata),
		Tags:                []string(row.Tags),
		LastActivityAt:      row.LastActivityAt.Time,
		CreatedAt:           row.CreatedAt.Time,
		UpdatedAt:           row.UpdatedAt.Time,
	}
	if row.CurrentVersionID.Valid {
		response.CurrentVersionID = pgvalue.MustUUIDValue(row.CurrentVersionID).String()
	}
	if row.LastWorkspaceMountID.Valid {
		response.LastWorkspaceMountID = pgvalue.MustUUIDValue(row.LastWorkspaceMountID).String()
	}
	response.AutoStopAt = optionalWorkspaceTime(row.AutoStopAt)
	response.AutoArchiveAt = optionalWorkspaceTime(row.AutoArchiveAt)
	response.AutoDeleteAt = optionalWorkspaceTime(row.AutoDeleteAt)
	response.ArchivedAt = optionalWorkspaceTime(row.ArchivedAt)
	response.DeletedAt = optionalWorkspaceTime(row.DeletedAt)
	return response
}

func parseWorkspaceUUID(field string, raw string) (pgtype.UUID, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return pgtype.UUID{}, fmt.Errorf("%s is required", field)
	}
	parsed, err := uuid.Parse(trimmed)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%s must be a UUID", field)
	}
	return pgvalue.UUID(parsed), nil
}

func optionalWorkspaceState(raw string) (db.NullWorkspaceState, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return db.NullWorkspaceState{}, nil
	}
	state := db.WorkspaceState(trimmed)
	switch state {
	case db.WorkspaceStateActive, db.WorkspaceStateDeleting, db.WorkspaceStateRecoveryRequired, db.WorkspaceStateArchived, db.WorkspaceStateDeleted:
		return db.NullWorkspaceState{WorkspaceState: state, Valid: true}, nil
	default:
		return db.NullWorkspaceState{}, errors.New("state must be active, deleting, recovery_required, archived, or deleted")
	}
}

func parseWorkspaceListLimit(r *http.Request) (int32, error) {
	limit, err := optionalLimitQuery(r, defaultWorkspaceListLimit)
	if err != nil {
		return 0, err
	}
	if limit > maxWorkspaceListLimit {
		return 0, fmt.Errorf("limit must be %d or less", maxWorkspaceListLimit)
	}
	return limit, nil
}

func optionalWorkspaceTime(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

func workspaceCreateFingerprint(request api.WorkspaceCreateRequest, metadata []byte, tags []string) (string, error) {
	payload, err := json.Marshal(struct {
		SandboxID    string          `json:"sandbox_id"`
		DeploymentID string          `json:"deployment_id"`
		ExternalID   string          `json:"external_id"`
		Metadata     json.RawMessage `json:"metadata"`
		Tags         []string        `json:"tags"`
	}{
		SandboxID:    strings.TrimSpace(request.SandboxID),
		DeploymentID: strings.TrimSpace(request.DeploymentID),
		ExternalID:   strings.TrimSpace(request.ExternalID),
		Metadata:     json.RawMessage(metadata),
		Tags:         tags,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func workspaceIdempotencyTTL(raw string) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultWorkspaceIdempotencyTTL, nil
	}
	ttl, err := time.ParseDuration(trimmed)
	if err != nil || ttl <= 0 {
		return 0, errors.New("idempotency_key_ttl must be a positive duration")
	}
	return ttl, nil
}

func (s *Server) writeWorkspaceError(w http.ResponseWriter, operation string, err error) {
	switch {
	case isNoRows(err):
		writeError(w, notFound(errors.New("workspace not found")))
	case isUniqueViolation(err):
		writeError(w, conflict(err))
	case errors.As(err, new(codedError)):
		var coded codedError
		if errors.As(err, &coded) && coded.code == "idempotency_pending" {
			w.Header().Set("Retry-After", "1")
			writeErrorStatus(w, http.StatusAccepted, err)
			return
		}
		if errors.As(err, &coded) && (coded.code == "idempotency_fingerprint_mismatch" || coded.code == "sandbox_not_deployed") {
			writeError(w, conflict(err))
			return
		}
		writeError(w, badRequest(err))
	default:
		s.log.Error(operation+" failed", "error", err)
		writeError(w, errors.New(operation))
	}
}
