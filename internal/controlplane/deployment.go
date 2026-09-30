package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

const (
	deploymentListDefaultLimit = int32(50)
	deploymentListMaxLimit     = int32(100)
)

type deploymentListCursor struct {
	ProjectID     string    `json:"project_id"`
	EnvironmentID string    `json:"environment_id"`
	CreatedAt     time.Time `json:"created_at"`
	ID            string    `json:"id"`
}

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	limit, cursor, err := parseDeploymentListQuery(r, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var after *deployment.Position
	if cursor != nil {
		after = &deployment.Position{CreatedAt: cursor.CreatedAt, ID: uuid.MustParse(cursor.ID)}
	}
	rows, hasMore, err := deployment.List(r.Context(), s.db, principal, scope, limit, after)
	if err != nil {
		s.writeDeploymentError(w, err)
		return
	}
	items := make([]api.DeploymentListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, api.DeploymentListItem{
			ID: pgvalue.UUIDString(row.ID), Version: row.Version,
			BundleDigest: row.BundleDigest, CreatedAt: pgvalue.Time(row.CreatedAt),
		})
	}
	response := api.ListDeploymentsResponse{Deployments: items}
	if hasMore {
		last := rows[len(rows)-1]
		response.NextCursor, err = encodeDeploymentListCursor(deploymentListCursor{
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			CreatedAt: pgvalue.Time(last.CreatedAt), ID: pgvalue.UUIDString(last.ID),
		})
		if err != nil {
			writeError(w, errors.New("list deployments"))
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func parseDeploymentListQuery(r *http.Request, projectID, environmentID string) (int32, *deploymentListCursor, error) {
	values := r.URL.Query()
	for name, entries := range values {
		if name != "cursor" && name != "limit" {
			return 0, nil, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || strings.TrimSpace(entries[0]) == "" {
			return 0, nil, fmt.Errorf("query parameter %q must appear once", name)
		}
	}
	limit := deploymentListDefaultLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > int64(deploymentListMaxLimit) {
			return 0, nil, errors.New("limit must be an integer in [1,100]")
		}
		limit = int32(parsed)
	}
	if raw := values.Get("cursor"); raw != "" {
		cursor, err := decodeDeploymentListCursor(raw)
		if err != nil {
			return 0, nil, err
		}
		if cursor.ProjectID != projectID || cursor.EnvironmentID != environmentID {
			return 0, nil, errors.New("deployment cursor does not match request scope")
		}
		return limit, &cursor, nil
	}
	return limit, nil, nil
}

func encodeDeploymentListCursor(cursor deploymentListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeDeploymentListCursor(raw string) (deploymentListCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return deploymentListCursor{}, errors.New("deployment cursor is invalid")
	}
	var cursor deploymentListCursor
	if json.Unmarshal(decoded, &cursor) != nil || cursor.ProjectID == "" ||
		cursor.EnvironmentID == "" || cursor.CreatedAt.IsZero() || ids.Validate(cursor.ID) != nil {
		return deploymentListCursor{}, errors.New("deployment cursor is invalid")
	}
	return cursor, nil
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) {
	deploymentID, err := parseUUIDParam(r, "deploymentID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	principal := principalFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	record, err := deployment.Get(r.Context(), s.db, principal, scope, deploymentID)
	if err != nil {
		s.writeDeploymentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deploymentResponse(record))
}

func (s *Server) getCurrentDeployment(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	record, err := deployment.GetCurrent(r.Context(), s.db, principal, scope)
	if err != nil {
		s.writeDeploymentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deploymentResponse(record))
}

func (s *Server) promoteDeployment(w http.ResponseWriter, r *http.Request) {
	deploymentID, err := parseUUIDParam(r, "deploymentID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	principal := principalFromContext(r.Context())
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	record, err := deployment.Promote(r.Context(), s.tx, principal, scope, deploymentID)
	if err != nil {
		s.writeDeploymentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deploymentResponse(record))
}

func deploymentResponse(record db.Deployment) api.DeploymentResponse {
	return api.DeploymentResponse{
		ID: pgvalue.UUIDString(record.ID), Version: record.Version,
		BundleDigest: record.BundleDigest, CreatedAt: pgvalue.Time(record.CreatedAt),
	}
}

// deploymentError maps errors of the deployment owner to HTTP errors.
func deploymentError(err error) error {
	var input deployment.InputError
	switch {
	case errors.As(err, &input):
		return badRequest(err)
	case errors.Is(err, deployment.ErrPermissionRequired):
		return forbidden(err)
	case errors.Is(err, deployment.ErrNotFound),
		errors.Is(err, deployment.ErrNotDeployable),
		errors.Is(err, deployment.ErrDefinitionNotFound):
		return notFound(err)
	case errors.Is(err, deployment.ErrNoCurrentDeployment):
		return notFound(codedError{code: "no_current_deployment", message: "no current deployment"})
	case errors.Is(err, deployment.ErrNoCurrentDefinitions):
		return notFound(codedError{code: "no_current_deployment", message: "Environment has no current Deployment"})
	case errors.Is(err, deployment.ErrSelectedDeploymentNotFound):
		return notFound(codedError{code: "deployment_not_found", message: "Deployment was not found"})
	case errors.Is(err, deployment.ErrDefinitionsNotMaterialized):
		return conflict(codedError{code: "deployment_not_materialized", message: "Deployment definitions are not materialized"})
	default:
		return err
	}
}

// writeDeploymentError writes a deployment owner error, logging the failures
// it does not describe to the client.
func (s *Server) writeDeploymentError(w http.ResponseWriter, err error) {
	mapped := deploymentError(err)
	if errorStatus(mapped) != http.StatusInternalServerError {
		writeError(w, mapped)
		return
	}
	s.log.Error("deployment request failed", "error", err)
	writeError(w, errors.New("deployment request"))
}
