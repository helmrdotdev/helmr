package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type environmentScopeReferenceError struct {
	message string
}

func (e environmentScopeReferenceError) Error() string {
	return e.message
}

func invalidEnvironmentScopeReference(message string) error {
	return environmentScopeReferenceError{message: message}
}

func isInvalidEnvironmentScopeReference(err error) bool {
	var referenceError environmentScopeReferenceError
	return errors.As(err, &referenceError)
}

func (s *Server) requestEnvironmentScope(ctx context.Context, actor auth.Actor, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	if actor.Kind == auth.ActorKindAPIKey {
		if projectID != "" || environmentID != "" {
			return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, invalidEnvironmentScopeReference("project_id and environment_id are not accepted with API keys")
		}
		scope, ok := actor.EnvironmentScope()
		if !ok {
			return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, errAPIKeyEnvironmentScopeRequired
		}
		scopeProjectID, scopeEnvironmentID, err := runScopeIDs(scope)
		if err != nil {
			return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
		}
		return scope, scopeProjectID, scopeEnvironmentID, nil
	}
	return s.secretRequestScope(ctx, actor.OrgID, projectID, environmentID)
}

func (s *Server) requestEnvironmentScopeFromRequest(r *http.Request, actor auth.Actor) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	projectID, environmentID, err := environmentScopeRefsFromRequest(r, actor)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return s.requestEnvironmentScope(r.Context(), actor, projectID, environmentID)
}

func environmentScopeRefsFromRequest(r *http.Request, actor auth.Actor) (string, string, error) {
	pathProjectID := chi.URLParam(r, "projectID")
	pathEnvironmentID := chi.URLParam(r, "environmentID")
	hasPathScope := pathProjectID != "" || pathEnvironmentID != ""
	if hasPathScope && (pathProjectID == "" || pathEnvironmentID == "") {
		return "", "", invalidEnvironmentScopeReference("project_id and environment_id must be provided together")
	}
	switch actor.Kind {
	case auth.ActorKindSession:
		if !hasPathScope {
			return "", "", invalidEnvironmentScopeReference("session environment scoped requests must use the project environment path")
		}
		return pathProjectID, pathEnvironmentID, nil
	case auth.ActorKindAPIKey:
		if hasPathScope {
			return "", "", invalidEnvironmentScopeReference("API key requests must use API key routes")
		}
		return "", "", nil
	}
	if hasPathScope {
		return pathProjectID, pathEnvironmentID, nil
	}
	return "", "", invalidEnvironmentScopeReference("environment scoped requests require a project environment path or an environment-bound API key")
}

func (s *Server) requestedRunListScope(r *http.Request, actor auth.Actor) (auth.Scope, error) {
	pathProjectID, pathEnvironmentID, err := environmentScopeRefsFromRequest(r, actor)
	if err != nil {
		return auth.Scope{}, err
	}
	if pathProjectID != "" || pathEnvironmentID != "" {
		scope, _, _, err := s.requestEnvironmentScope(r.Context(), actor, pathProjectID, pathEnvironmentID)
		return scope, err
	}
	if actor.Kind == auth.ActorKindAPIKey {
		scope, ok := actor.EnvironmentScope()
		if !ok {
			return auth.Scope{}, errAPIKeyEnvironmentScopeRequired
		}
		return scope, nil
	}
	return auth.Scope{}, invalidEnvironmentScopeReference("session environment scoped requests must use the project environment path")
}

func runScopeIDs(scope auth.Scope) (pgtype.UUID, pgtype.UUID, error) {
	projectID, err := ids.Parse(scope.ProjectID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	environmentID, err := ids.Parse(scope.EnvironmentID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	return pgvalue.UUID(projectID), pgvalue.UUID(environmentID), nil
}

func (s *Server) normalizeProjectEnvironmentScope(ctx context.Context, orgID uuid.UUID, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	parsedProjectID, err := ids.Parse(projectID)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, invalidEnvironmentScopeReference("project_id must be a canonical project UUIDv7")
	}
	project, err := s.db.GetProject(ctx, db.GetProjectParams{OrgID: pgvalue.UUID(orgID), ID: pgvalue.UUID(parsedProjectID)})
	if isNoRows(err) {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, invalidEnvironmentScopeReference("project_id must reference an active project")
	}
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("load project: %w", err)
	}
	parsedEnvironmentID, err := ids.Parse(environmentID)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, invalidEnvironmentScopeReference("environment_id must be a canonical environment UUIDv7")
	}
	environment, err := s.db.GetEnvironment(ctx, db.GetEnvironmentParams{OrgID: pgvalue.UUID(orgID), ProjectID: project.ID, ID: pgvalue.UUID(parsedEnvironmentID)})
	if isNoRows(err) {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, invalidEnvironmentScopeReference("environment_id must reference an active environment")
	}
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("load environment: %w", err)
	}
	return auth.Scope{OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID}, project.ID, environment.ID, nil
}

func (s *Server) secretRequestScope(ctx context.Context, orgID uuid.UUID, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	scope, _, _, err := s.normalizeProjectEnvironmentScope(ctx, orgID, projectID, environmentID)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	scopeProjectID, scopeEnvironmentID, err := runScopeIDs(scope)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return scope, scopeProjectID, scopeEnvironmentID, nil
}
