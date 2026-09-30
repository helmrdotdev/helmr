package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/ids"
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
	s.listDefinitions(w, r, definition.KindTask)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	s.getDefinition(w, r, definition.KindTask, chi.URLParam(r, "taskID"))
}

func (s *Server) listActors(w http.ResponseWriter, r *http.Request) {
	s.listDefinitions(w, r, definition.KindActor)
}

func (s *Server) getActor(w http.ResponseWriter, r *http.Request) {
	s.getDefinition(w, r, definition.KindActor, chi.URLParam(r, "actorID"))
}

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request) {
	s.listDefinitions(w, r, definition.KindSandbox)
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request) {
	s.getDefinition(w, r, definition.KindSandbox, chi.URLParam(r, "sandboxID"))
}

func (s *Server) listDefinitions(w http.ResponseWriter, r *http.Request, kind definition.Kind) {
	principal := principalFromContext(r.Context())
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	selected, cursor, limit, err := parseDefinitionListQuery(r, string(kind), scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var after *string
	if cursor != nil {
		cursorDeploymentID := uuid.MustParse(cursor.DeploymentID)
		selected, after = &cursorDeploymentID, &cursor.AfterID
	}
	page, err := deployment.ListDefinitions(r.Context(), s.db, principal, scope, kind, selected, limit, after)
	if err != nil {
		s.writeDeploymentError(w, err)
		return
	}
	deploymentID := page.DeploymentID.String()
	nextCursor := ""
	if page.HasMore {
		nextCursor, err = encodeDefinitionListCursor(definitionListCursor{
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			DeploymentID: deploymentID, Kind: string(kind), AfterID: page.DeclaredIDs[len(page.DeclaredIDs)-1],
		})
		if err != nil {
			writeError(w, errors.New("list Definitions"))
			return
		}
	}
	writeDefinitionList(w, kind, deploymentID, page.DeclaredIDs, nextCursor)
}

func (s *Server) getDefinition(w http.ResponseWriter, r *http.Request, kind definition.Kind, id string) {
	if err := api.ValidateDefinitionID(id); err != nil {
		writeError(w, badRequest(err))
		return
	}
	principal := principalFromContext(r.Context())
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	selected, err := parseDefinitionItemQuery(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	deploymentID, declaredID, err := deployment.GetDefinition(r.Context(), s.db, principal, scope, kind, selected, id)
	if err != nil {
		s.writeDeploymentError(w, err)
		return
	}
	writeDefinition(w, kind, declaredID, deploymentID.String())
}

func parseDefinitionListQuery(
	r *http.Request,
	kind, projectID, environmentID string,
) (*uuid.UUID, *definitionListCursor, int32, error) {
	values := r.URL.Query()
	for name, entries := range values {
		if name != "deployment_id" && name != "cursor" && name != "limit" {
			return nil, nil, 0, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || entries[0] == "" {
			return nil, nil, 0, fmt.Errorf("%s must appear once", name)
		}
	}
	limit := definitionListDefaultLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > int64(definitionListMaxLimit) {
			return nil, nil, 0, errors.New("limit must be an integer in [1,100]")
		}
		limit = int32(parsed)
	}
	selector, err := parseOptionalDefinitionDeploymentID(values.Get("deployment_id"))
	if err != nil {
		return nil, nil, 0, err
	}
	if raw := values.Get("cursor"); raw != "" {
		cursor, err := decodeDefinitionListCursor(raw)
		if err != nil {
			return nil, nil, 0, err
		}
		if cursor.ProjectID != projectID || cursor.EnvironmentID != environmentID || cursor.Kind != kind {
			return nil, nil, 0, errors.New("definition cursor does not match request scope")
		}
		if selector != nil && selector.String() != cursor.DeploymentID {
			return nil, nil, 0, errors.New("deployment_id does not match definition cursor")
		}
		return selector, &cursor, limit, nil
	}
	return selector, nil, limit, nil
}

func parseDefinitionItemQuery(r *http.Request) (*uuid.UUID, error) {
	values := r.URL.Query()
	for name, entries := range values {
		if name != "deployment_id" {
			return nil, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || entries[0] == "" {
			return nil, errors.New("deployment_id must appear once")
		}
	}
	return parseOptionalDefinitionDeploymentID(values.Get("deployment_id"))
}

func parseOptionalDefinitionDeploymentID(raw string) (*uuid.UUID, error) {
	if raw == "" {
		return nil, nil
	}
	id, err := ids.Parse(raw)
	if err != nil {
		return nil, errors.New("deployment_id must be a canonical UUIDv7")
	}
	return &id, nil
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

func writeDefinitionList(w http.ResponseWriter, kind definition.Kind, deploymentID string, ids []string, nextCursor string) {
	switch kind {
	case definition.KindTask:
		items := make([]api.DefinitionListItem, 0, len(ids))
		for _, id := range ids {
			items = append(items, api.DefinitionListItem{ID: id})
		}
		writeJSON(w, http.StatusOK, api.ListTasksResponse{DeploymentID: deploymentID, Tasks: items, NextCursor: nextCursor})
	case definition.KindActor:
		items := make([]api.DefinitionListItem, 0, len(ids))
		for _, id := range ids {
			items = append(items, api.DefinitionListItem{ID: id})
		}
		writeJSON(w, http.StatusOK, api.ListActorsResponse{DeploymentID: deploymentID, Actors: items, NextCursor: nextCursor})
	case definition.KindSandbox:
		items := make([]api.DefinitionListItem, 0, len(ids))
		for _, id := range ids {
			items = append(items, api.DefinitionListItem{ID: id})
		}
		writeJSON(w, http.StatusOK, api.ListSandboxesResponse{DeploymentID: deploymentID, Sandboxes: items, NextCursor: nextCursor})
	}
}

func writeDefinition(w http.ResponseWriter, kind definition.Kind, id, deploymentID string) {
	switch kind {
	case definition.KindTask:
		writeJSON(w, http.StatusOK, api.Task{ID: id, DeploymentID: deploymentID})
	case definition.KindActor:
		writeJSON(w, http.StatusOK, api.Actor{ID: id, DeploymentID: deploymentID})
	case definition.KindSandbox:
		writeJSON(w, http.StatusOK, api.Sandbox{ID: id, DeploymentID: deploymentID})
	}
}
