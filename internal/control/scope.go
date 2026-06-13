package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) requestEnvironmentScope(ctx context.Context, actor auth.Actor, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	projectID = strings.TrimSpace(projectID)
	environmentID = strings.TrimSpace(environmentID)
	if actor.Kind == auth.ActorKindAPIKey {
		if projectID != "" || environmentID != "" {
			return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, errors.New("project_id and environment_id are not accepted with API keys")
		}
		scope, ok := actor.EnvironmentScope()
		if !ok {
			return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, errors.New("API key is not bound to an environment")
		}
		scopeProjectID, scopeEnvironmentID, err := s.runScopeIDs(ctx, actor.OrgID, scope)
		if err != nil {
			return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
		}
		return scope, scopeProjectID, scopeEnvironmentID, nil
	}
	return s.secretRequestScope(ctx, actor.OrgID, projectID, environmentID)
}

func (s *Server) requestEnvironmentScopeFromRequest(r *http.Request, actor auth.Actor, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	projectID, environmentID, err := environmentScopeRefsFromRequest(r, actor, projectID, environmentID)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return s.requestEnvironmentScope(r.Context(), actor, projectID, environmentID)
}

func environmentScopeRefsFromRequest(r *http.Request, actor auth.Actor, projectID string, environmentID string) (string, string, error) {
	projectID = strings.TrimSpace(projectID)
	environmentID = strings.TrimSpace(environmentID)
	pathProjectID := strings.TrimSpace(chi.URLParam(r, "projectID"))
	pathEnvironmentID := strings.TrimSpace(chi.URLParam(r, "environmentID"))
	hasPathScope := pathProjectID != "" || pathEnvironmentID != ""
	if hasPathScope && (pathProjectID == "" || pathEnvironmentID == "") {
		return "", "", errors.New("project_id and environment_id must be provided together")
	}
	switch actor.Kind {
	case auth.ActorKindSession:
		if !hasPathScope {
			return "", "", errors.New("session environment scoped requests must use the project environment path")
		}
		if projectID != "" || environmentID != "" {
			return "", "", errors.New("project_id and environment_id are not accepted in session request payloads")
		}
		return pathProjectID, pathEnvironmentID, nil
	case auth.ActorKindAPIKey:
		if hasPathScope {
			return "", "", errors.New("API key requests must use API key routes")
		}
		if projectID != "" || environmentID != "" {
			return "", "", errors.New("project_id and environment_id are not accepted with API keys")
		}
	}
	return projectID, environmentID, nil
}

func (s *Server) requireActorScopeForRecord(r *http.Request, actor auth.Actor, projectID pgtype.UUID, environmentID pgtype.UUID) error {
	switch actor.Kind {
	case auth.ActorKindSession:
		_, pathProjectID, pathEnvironmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
		if err != nil {
			return err
		}
		if pathProjectID != projectID || pathEnvironmentID != environmentID {
			return errRecordNotFound
		}
	case auth.ActorKindAPIKey:
		scope, ok := actor.EnvironmentScope()
		if !ok {
			return errors.New("API key is not bound to an environment")
		}
		recordScope := auth.Scope{
			OrgID:         actor.OrgID,
			ProjectID:     ids.MustFromPG(projectID).String(),
			EnvironmentID: ids.MustFromPG(environmentID).String(),
		}
		if scope.ProjectID != recordScope.ProjectID || scope.EnvironmentID != recordScope.EnvironmentID {
			return errRecordNotFound
		}
	default:
		return nil
	}
	return nil
}

func isScopeRequestError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "project_id") || strings.Contains(message, "environment_id")
}

func (s *Server) requestedRunListScope(r *http.Request, actor auth.Actor) (auth.Scope, error) {
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	environmentID := strings.TrimSpace(r.URL.Query().Get("environment_id"))
	pathProjectID, pathEnvironmentID, err := environmentScopeRefsFromRequest(r, actor, projectID, environmentID)
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
			return auth.Scope{}, errors.New("API key is not bound to an environment")
		}
		return scope, nil
	}
	return auth.Scope{}, errors.New("session environment scoped requests must use the project environment path")
}

func (s *Server) runScopeIDs(ctx context.Context, orgID uuid.UUID, scope auth.Scope) (pgtype.UUID, pgtype.UUID, error) {
	projectID, err := ids.Parse(scope.ProjectID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	environmentID, err := ids.Parse(scope.EnvironmentID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	return ids.ToPG(projectID), ids.ToPG(environmentID), nil
}

func (s *Server) normalizeProjectEnvironmentScope(ctx context.Context, orgID uuid.UUID, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	project, err := s.resolveProjectRef(ctx, orgID, projectID)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	environment, err := s.resolveEnvironmentRef(ctx, orgID, project.ID, environmentID)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return auth.Scope{OrgID: orgID, ProjectID: ids.MustFromPG(project.ID).String(), EnvironmentID: ids.MustFromPG(environment.ID).String()}, project.ID, environment.ID, nil
}

func (s *Server) resolveProjectRef(ctx context.Context, orgID uuid.UUID, projectRef string) (db.Project, error) {
	projectRef = strings.TrimSpace(projectRef)
	if projectRef == "" {
		defaultScope, err := s.db.GetDefaultProjectEnvironment(ctx, ids.ToPG(orgID))
		if err != nil {
			return db.Project{}, fmt.Errorf("load project selection: %w", err)
		}
		return s.db.GetProject(ctx, db.GetProjectParams{OrgID: ids.ToPG(orgID), ID: defaultScope.ProjectID})
	}
	if parsed, err := ids.Parse(projectRef); err == nil {
		project, err := s.db.GetProject(ctx, db.GetProjectParams{OrgID: ids.ToPG(orgID), ID: ids.ToPG(parsed)})
		if isNoRows(err) {
			return db.Project{}, errors.New("project_id must reference an active project")
		}
		if err != nil {
			return db.Project{}, fmt.Errorf("load project: %w", err)
		}
		return project, nil
	}
	project, err := s.db.GetProjectBySlug(ctx, db.GetProjectBySlugParams{OrgID: ids.ToPG(orgID), Slug: strings.ToLower(projectRef)})
	if isNoRows(err) {
		return db.Project{}, errors.New("project_id must be a project UUID or a project slug")
	}
	if err != nil {
		return db.Project{}, fmt.Errorf("load project: %w", err)
	}
	return project, nil
}

func (s *Server) resolveEnvironmentRef(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentRef string) (db.Environment, error) {
	environmentRef = strings.TrimSpace(environmentRef)
	if environmentRef == "" {
		environment, err := s.db.GetDefaultEnvironment(ctx, db.GetDefaultEnvironmentParams{OrgID: ids.ToPG(orgID), ProjectID: projectID})
		if isNoRows(err) {
			return db.Environment{}, errors.New("environment_id must reference an active environment")
		}
		if err != nil {
			return db.Environment{}, fmt.Errorf("load environment: %w", err)
		}
		return environment, nil
	}
	if parsed, err := ids.Parse(environmentRef); err == nil {
		environment, err := s.db.GetEnvironment(ctx, db.GetEnvironmentParams{OrgID: ids.ToPG(orgID), ProjectID: projectID, ID: ids.ToPG(parsed)})
		if isNoRows(err) {
			return db.Environment{}, errors.New("environment_id must reference an active environment")
		}
		if err != nil {
			return db.Environment{}, fmt.Errorf("load environment: %w", err)
		}
		return environment, nil
	}
	environment, err := s.db.GetEnvironmentBySlug(ctx, db.GetEnvironmentBySlugParams{OrgID: ids.ToPG(orgID), ProjectID: projectID, Slug: strings.ToLower(environmentRef)})
	if isNoRows(err) {
		return db.Environment{}, errors.New("environment_id must be an environment UUID or an environment slug")
	}
	if err != nil {
		return db.Environment{}, fmt.Errorf("load environment: %w", err)
	}
	return environment, nil
}

func (s *Server) secretRequestScope(ctx context.Context, orgID uuid.UUID, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	projectID = strings.TrimSpace(projectID)
	environmentID = strings.TrimSpace(environmentID)
	scope, _, _, err := s.normalizeProjectEnvironmentScope(ctx, orgID, projectID, environmentID)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	scopeProjectID, scopeEnvironmentID, err := s.runScopeIDs(ctx, orgID, scope)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return scope, scopeProjectID, scopeEnvironmentID, nil
}
