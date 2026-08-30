package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

type currentDeploymentStore interface {
	GetCurrentDeployment(context.Context, db.GetCurrentDeploymentParams) (db.Deployment, error)
}

type deploymentStatusStore interface {
	GetDeployment(context.Context, db.GetDeploymentParams) (db.Deployment, error)
	GetDeploymentForOrg(context.Context, db.GetDeploymentForOrgParams) (db.Deployment, error)
	ListScopedDeployments(context.Context, db.ListScopedDeploymentsParams) ([]db.ListScopedDeploymentsRow, error)
}

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
	store, ok := s.db.(deploymentStatusStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) && !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		writeError(w, errors.New("list deployments"))
		return
	}
	limit, cursor, err := parseDeploymentListQuery(r, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	params := db.ListScopedDeploymentsParams{
		OrgID: pgvalue.UUID(actor.OrgID), ProjectID: projectID,
		EnvironmentID: environmentID, RowLimit: limit + 1,
	}
	if cursor != nil {
		params.HasAfter = true
		params.AfterCreatedAt = pgvalue.Timestamptz(cursor.CreatedAt)
		params.AfterID = pgvalue.UUID(uuid.MustParse(cursor.ID))
	}
	rows, err := store.ListScopedDeployments(r.Context(), params)
	if err != nil {
		s.log.Error("list deployments failed", "error", err)
		writeError(w, errors.New("list deployments"))
		return
	}
	hasMore := len(rows) > int(limit)
	if hasMore {
		rows = rows[:limit]
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
	store, ok := s.db.(deploymentStatusStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	deploymentID, err := parseUUIDParam(r, "deploymentID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) && !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		writeError(w, errors.New("get deployment"))
		return
	}
	record, err := store.GetDeploymentForOrg(r.Context(), db.GetDeploymentForOrgParams{
		OrgID: pgvalue.UUID(actor.OrgID), ID: pgvalue.UUID(deploymentID),
	})
	if isNoRows(err) || (err == nil && (record.ProjectID != projectID || record.EnvironmentID != environmentID)) {
		writeError(w, notFound(errors.New("deployment not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("get deployment"))
		return
	}
	writeJSON(w, http.StatusOK, deploymentResponse(record))
}

func (s *Server) getCurrentDeployment(w http.ResponseWriter, r *http.Request) {
	store, ok := s.db.(currentDeploymentStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		writeError(w, errors.New("get current deployment"))
		return
	}
	record, err := store.GetCurrentDeployment(r.Context(), db.GetCurrentDeploymentParams{
		OrgID: pgvalue.UUID(actor.OrgID), ProjectID: projectID, EnvironmentID: environmentID,
	})
	if isNoRows(err) {
		writeError(w, notFound(codedError{code: "no_current_deployment", message: "no current deployment"}))
		return
	}
	if err != nil {
		writeError(w, errors.New("get current deployment"))
		return
	}
	writeJSON(w, http.StatusOK, deploymentResponse(record))
}

func deploymentVersion(id uuid.UUID) string {
	milliseconds := int64(binary.BigEndian.Uint64(id[:]) >> 16)
	return time.UnixMilli(milliseconds).UTC().Format("20060102") + "." + id.String()
}

func deploymentResponse(record db.Deployment) api.DeploymentResponse {
	return api.DeploymentResponse{
		ID: pgvalue.UUIDString(record.ID), Version: record.Version,
		BundleDigest: record.BundleDigest, CreatedAt: pgvalue.Time(record.CreatedAt),
	}
}

func writeDeploymentError(w http.ResponseWriter, s *Server, err error) {
	var idempotencyConflict idempotency.ConflictError
	if errors.As(err, &idempotencyConflict) {
		writeError(w, conflict(errors.New("idempotency key conflicts with another deployment bundle")))
		return
	}
	if errorStatus(err) != http.StatusInternalServerError {
		writeError(w, err)
		return
	}
	s.log.Error("deployment request failed", "error", err)
	writeError(w, errors.New("deployment request"))
}
