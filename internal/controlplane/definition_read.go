package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	definitionListDefaultLimit = int32(50)
	definitionListMaxLimit     = int32(100)
)

type definitionListCursor struct {
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
	DeploymentID  string `json:"deployment_id"`
	Kind          string `json:"kind"`
	AfterID       string `json:"after_id"`
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	s.listDefinitions(w, r, "task")
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	s.getDefinition(w, r, "task", chi.URLParam(r, "taskID"))
}

func (s *Server) listActors(w http.ResponseWriter, r *http.Request) {
	s.listDefinitions(w, r, "actor")
}

func (s *Server) getActor(w http.ResponseWriter, r *http.Request) {
	s.getDefinition(w, r, "actor", chi.URLParam(r, "actorID"))
}

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request) {
	s.listDefinitions(w, r, "sandbox")
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request) {
	s.getDefinition(w, r, "sandbox", chi.URLParam(r, "sandboxID"))
}

func (s *Server) listDefinitions(w http.ResponseWriter, r *http.Request, kind string) {
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !canReadDeployments(principal, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	selector, cursor, limit, err := parseDefinitionListQuery(r, kind, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if cursor != nil {
		selector = pgvalue.UUID(uuid.MustParse(cursor.DeploymentID))
	}
	deploymentID, err := s.resolveDefinitionDeployment(
		r.Context(), selector, pgvalue.UUID(principal.OrgID), projectID, environmentID,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	resolvedDeploymentID := pgvalue.UUIDString(deploymentID)
	if cursor != nil && cursor.DeploymentID != resolvedDeploymentID {
		writeError(w, badRequest(errors.New("definition cursor deployment does not match")))
		return
	}
	afterID := ""
	if cursor != nil {
		afterID = cursor.AfterID
	}
	rows, err := s.db.ListDefinitionSnapshots(r.Context(), db.ListDefinitionSnapshotsParams{
		EnvironmentID: environmentID, DeploymentID: deploymentID, Kind: kind,
		HasAfter: cursor != nil, AfterID: afterID, RowLimit: limit + 1,
	})
	if err != nil {
		s.log.Error("list Definitions failed", "kind", kind, "error", err)
		writeError(w, errors.New("list definitions"))
		return
	}
	hasMore := len(rows) > int(limit)
	if hasMore {
		rows = rows[:limit]
	}
	nextCursor := ""
	if hasMore {
		nextCursor, err = encodeDefinitionListCursor(definitionListCursor{
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			DeploymentID: resolvedDeploymentID, Kind: kind, AfterID: rows[len(rows)-1],
		})
		if err != nil {
			writeError(w, errors.New("list Definitions"))
			return
		}
	}
	writeDefinitionList(w, kind, resolvedDeploymentID, rows, nextCursor)
}

func (s *Server) getDefinition(w http.ResponseWriter, r *http.Request, kind, id string) {
	if err := api.ValidateDefinitionID(id); err != nil {
		writeError(w, badRequest(err))
		return
	}
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !canReadDeployments(principal, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	selector, err := parseDefinitionItemQuery(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	deploymentID, err := s.resolveDefinitionDeployment(
		r.Context(), selector, pgvalue.UUID(principal.OrgID), projectID, environmentID,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	declaredID, err := s.db.GetDefinitionSnapshot(r.Context(), db.GetDefinitionSnapshotParams{
		EnvironmentID: environmentID, DeploymentID: deploymentID, Kind: kind, DeclaredID: id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("definition not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("get definition"))
		return
	}
	writeDefinition(w, kind, declaredID, pgvalue.UUIDString(deploymentID))
}

func (s *Server) resolveDefinitionDeployment(
	ctx context.Context,
	selector pgtype.UUID,
	orgID, projectID, environmentID pgtype.UUID,
) (pgtype.UUID, error) {
	var (
		deployment db.Deployment
		err        error
	)
	if selector.Valid {
		deployment, err = s.db.GetDeployment(ctx, db.GetDeploymentParams{
			OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID, ID: selector,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, notFound(codedError{
				code: "deployment_not_found", message: "Deployment was not found",
			})
		}
	} else {
		deployment, err = s.db.GetCurrentDeploymentForRoute(ctx, db.GetCurrentDeploymentForRouteParams{
			OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, notFound(codedError{
				code: "no_current_deployment", message: "Environment has no current Deployment",
			})
		}
	}
	if err != nil {
		return pgtype.UUID{}, errors.New("resolve definition deployment")
	}
	if !deployment.ProgramArtifactID.Valid || len(deployment.ProgramIndexDigest) == 0 ||
		deployment.RuntimeArtifactDigest == "" {
		return pgtype.UUID{}, conflict(codedError{
			code: "deployment_not_materialized", message: "Deployment definitions are not materialized",
		})
	}
	return deployment.ID, nil
}

func canReadDeployments(principal auth.Actor, scope auth.Scope) bool {
	return principal.HasPermission(auth.PermissionTasksDeploy, scope) ||
		principal.HasPermission(auth.PermissionRunsRead, scope)
}

func parseDefinitionListQuery(
	r *http.Request,
	kind, projectID, environmentID string,
) (pgtype.UUID, *definitionListCursor, int32, error) {
	values := r.URL.Query()
	for name, entries := range values {
		if name != "deployment_id" && name != "cursor" && name != "limit" {
			return pgtype.UUID{}, nil, 0, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || entries[0] == "" {
			return pgtype.UUID{}, nil, 0, fmt.Errorf("%s must appear once", name)
		}
	}
	limit := definitionListDefaultLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > int64(definitionListMaxLimit) {
			return pgtype.UUID{}, nil, 0, errors.New("limit must be an integer in [1,100]")
		}
		limit = int32(parsed)
	}
	selector, err := parseOptionalDefinitionDeploymentID(values.Get("deployment_id"))
	if err != nil {
		return pgtype.UUID{}, nil, 0, err
	}
	if raw := values.Get("cursor"); raw != "" {
		cursor, err := decodeDefinitionListCursor(raw)
		if err != nil {
			return pgtype.UUID{}, nil, 0, err
		}
		if cursor.ProjectID != projectID || cursor.EnvironmentID != environmentID || cursor.Kind != kind {
			return pgtype.UUID{}, nil, 0, errors.New("definition cursor does not match request scope")
		}
		if selector.Valid && pgvalue.UUIDString(selector) != cursor.DeploymentID {
			return pgtype.UUID{}, nil, 0, errors.New("deployment_id does not match definition cursor")
		}
		return selector, &cursor, limit, nil
	}
	return selector, nil, limit, nil
}

func parseDefinitionItemQuery(r *http.Request) (pgtype.UUID, error) {
	values := r.URL.Query()
	for name, entries := range values {
		if name != "deployment_id" {
			return pgtype.UUID{}, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || entries[0] == "" {
			return pgtype.UUID{}, errors.New("deployment_id must appear once")
		}
	}
	return parseOptionalDefinitionDeploymentID(values.Get("deployment_id"))
}

func parseOptionalDefinitionDeploymentID(raw string) (pgtype.UUID, error) {
	if raw == "" {
		return pgtype.UUID{}, nil
	}
	id, err := ids.Parse(raw)
	if err != nil {
		return pgtype.UUID{}, errors.New("deployment_id must be a canonical UUIDv7")
	}
	return pgvalue.UUID(id), nil
}

func encodeDefinitionListCursor(cursor definitionListCursor) (string, error) {
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeDefinitionListCursor(raw string) (definitionListCursor, error) {
	encoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return definitionListCursor{}, errors.New("definition cursor is invalid")
	}
	var cursor definitionListCursor
	if err := json.Unmarshal(encoded, &cursor); err != nil {
		return definitionListCursor{}, errors.New("definition cursor is invalid")
	}
	if cursor.ProjectID == "" || cursor.EnvironmentID == "" || cursor.Kind == "" || cursor.AfterID == "" {
		return definitionListCursor{}, errors.New("definition cursor is invalid")
	}
	if err := ids.Validate(cursor.DeploymentID); err != nil {
		return definitionListCursor{}, errors.New("definition cursor is invalid")
	}
	if err := api.ValidateDefinitionID(cursor.AfterID); err != nil {
		return definitionListCursor{}, errors.New("definition cursor is invalid")
	}
	return cursor, nil
}

func writeDefinitionList(w http.ResponseWriter, kind, deploymentID string, ids []string, nextCursor string) {
	switch kind {
	case "task":
		items := make([]api.DefinitionListItem, 0, len(ids))
		for _, id := range ids {
			items = append(items, api.DefinitionListItem{ID: id})
		}
		writeJSON(w, http.StatusOK, api.ListTasksResponse{DeploymentID: deploymentID, Tasks: items, NextCursor: nextCursor})
	case "actor":
		items := make([]api.DefinitionListItem, 0, len(ids))
		for _, id := range ids {
			items = append(items, api.DefinitionListItem{ID: id})
		}
		writeJSON(w, http.StatusOK, api.ListActorsResponse{DeploymentID: deploymentID, Actors: items, NextCursor: nextCursor})
	case "sandbox":
		items := make([]api.DefinitionListItem, 0, len(ids))
		for _, id := range ids {
			items = append(items, api.DefinitionListItem{ID: id})
		}
		writeJSON(w, http.StatusOK, api.ListSandboxesResponse{DeploymentID: deploymentID, Sandboxes: items, NextCursor: nextCursor})
	}
}

func writeDefinition(w http.ResponseWriter, kind, id, deploymentID string) {
	switch kind {
	case "task":
		writeJSON(w, http.StatusOK, api.Task{ID: id, DeploymentID: deploymentID})
	case "actor":
		writeJSON(w, http.StatusOK, api.Actor{ID: id, DeploymentID: deploymentID})
	case "sandbox":
		writeJSON(w, http.StatusOK, api.Sandbox{ID: id, DeploymentID: deploymentID})
	}
}
