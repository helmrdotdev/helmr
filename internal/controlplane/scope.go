package controlplane

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/org"
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

func (s *Server) requestEnvironmentScope(ctx context.Context, principal auth.Principal, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	if principal.Kind == auth.PrincipalKindAPIKey {
		if projectID != "" || environmentID != "" {
			return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, invalidEnvironmentScopeReference("project_id and environment_id are not accepted with API keys")
		}
		scope, ok := principal.EnvironmentScope()
		if !ok {
			return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, errAPIKeyEnvironmentScopeRequired
		}
		scopeProjectID, scopeEnvironmentID, err := runScopeIDs(scope)
		if err != nil {
			return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
		}
		return scope, scopeProjectID, scopeEnvironmentID, nil
	}
	scope, err := org.ResolveEnvironmentScope(ctx, s.db, principal.OrgID, projectID, environmentID)
	var input org.InputError
	if errors.As(err, &input) {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, invalidEnvironmentScopeReference(input.Error())
	}
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	scopeProjectID, scopeEnvironmentID, err := runScopeIDs(scope)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return scope, scopeProjectID, scopeEnvironmentID, nil
}

func (s *Server) requestEnvironmentScopeFromRequest(r *http.Request, principal auth.Principal) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	projectID, environmentID, err := environmentScopeRefsFromRequest(r, principal)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return s.requestEnvironmentScope(r.Context(), principal, projectID, environmentID)
}

func environmentScopeRefsFromRequest(r *http.Request, principal auth.Principal) (string, string, error) {
	pathProjectID := chi.URLParam(r, "projectID")
	pathEnvironmentID := chi.URLParam(r, "environmentID")
	hasPathScope := pathProjectID != "" || pathEnvironmentID != ""
	if hasPathScope && (pathProjectID == "" || pathEnvironmentID == "") {
		return "", "", invalidEnvironmentScopeReference("project_id and environment_id must be provided together")
	}
	switch principal.Kind {
	case auth.PrincipalKindSession:
		if !hasPathScope {
			return "", "", invalidEnvironmentScopeReference("session environment scoped requests must use the project environment path")
		}
		return pathProjectID, pathEnvironmentID, nil
	case auth.PrincipalKindAPIKey:
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

func (s *Server) requestedRunListScope(r *http.Request, principal auth.Principal) (auth.Scope, error) {
	pathProjectID, pathEnvironmentID, err := environmentScopeRefsFromRequest(r, principal)
	if err != nil {
		return auth.Scope{}, err
	}
	if pathProjectID != "" || pathEnvironmentID != "" {
		scope, _, _, err := s.requestEnvironmentScope(r.Context(), principal, pathProjectID, pathEnvironmentID)
		return scope, err
	}
	if principal.Kind == auth.PrincipalKindAPIKey {
		scope, ok := principal.EnvironmentScope()
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
