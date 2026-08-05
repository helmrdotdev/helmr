package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

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
	workspaceListDefaultLimit = int32(50)
	workspaceListMaxLimit     = int32(100)
	workspaceListCursorPrefix = "ws1."
)

type workspaceListCursor struct {
	ProjectID     string    `json:"project_id"`
	EnvironmentID string    `json:"environment_id"`
	CreatedAt     time.Time `json:"created_at"`
	ID            string    `json:"id"`
}

func (s *Server) listWorkspacesHTTP(w http.ResponseWriter, r *http.Request) {
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_workspace_reference", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionWorkspacesRead, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	limit, cursor, exactKey, err := parseWorkspaceListQuery(r, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_workspace_reference", message: err.Error()}))
		return
	}
	response := api.ListWorkspacesResponse{Workspaces: []api.WorkspaceSnapshot{}}
	if exactKey != nil {
		record, err := s.db.GetWorkspaceByKey(r.Context(), db.GetWorkspaceByKeyParams{
			OrgID: pgvalue.UUID(principal.OrgID), ProjectID: projectID,
			EnvironmentID: environmentID, Key: pgvalue.Text(*exactKey),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, response)
			return
		}
		if err != nil {
			writeError(w, unavailable(codedError{code: "workspace_authority_unavailable", message: "workspace authority is unavailable", retryable: true}))
			return
		}
		snapshot, err := s.workspaceSnapshot(r.Context(), s.db, record)
		if err != nil {
			writeError(w, unavailable(codedError{code: "workspace_authority_unavailable", message: "workspace authority is unavailable", retryable: true}))
			return
		}
		response.Workspaces = append(response.Workspaces, snapshot)
		writeJSON(w, http.StatusOK, response)
		return
	}
	params := db.ListWorkspaceSnapshotsParams{
		OrgID: pgvalue.UUID(principal.OrgID), ProjectID: projectID, EnvironmentID: environmentID,
		RowLimit: limit + 1,
	}
	if cursor != nil {
		params.HasAfter = true
		params.AfterCreatedAt = pgtype.Timestamptz{Time: cursor.CreatedAt, Valid: true}
		params.AfterID = pgvalue.UUID(uuid.MustParse(cursor.ID))
	}
	rows, err := s.db.ListWorkspaceSnapshots(r.Context(), params)
	if err != nil {
		writeError(w, unavailable(codedError{code: "workspace_authority_unavailable", message: "workspace authority is unavailable", retryable: true}))
		return
	}
	hasMore := len(rows) > int(limit)
	if hasMore {
		rows = rows[:limit]
	}
	for _, row := range rows {
		snapshot, err := s.workspaceSnapshot(r.Context(), s.db, row)
		if err != nil {
			writeError(w, unavailable(codedError{code: "workspace_authority_unavailable", message: "workspace authority is unavailable", retryable: true}))
			return
		}
		response.Workspaces = append(response.Workspaces, snapshot)
	}
	if hasMore {
		last := rows[len(rows)-1]
		response.NextCursor, err = encodeWorkspaceListCursor(workspaceListCursor{
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			CreatedAt: pgvalue.Time(last.CreatedAt), ID: pgvalue.UUIDString(last.ID),
		})
		if err != nil {
			writeError(w, errors.New("list Workspaces"))
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func parseWorkspaceListQuery(
	r *http.Request,
	projectID, environmentID string,
) (int32, *workspaceListCursor, *string, error) {
	values := r.URL.Query()
	for name, entries := range values {
		if name != "key" && name != "cursor" && name != "limit" {
			return 0, nil, nil, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || entries[0] == "" {
			return 0, nil, nil, fmt.Errorf("%s must appear once", name)
		}
	}
	if raw := values.Get("key"); raw != "" {
		if values.Get("cursor") != "" || values.Get("limit") != "" {
			return 0, nil, nil, errors.New("workspace exact key lookup does not accept cursor or limit")
		}
		if err := validateWorkspaceKey(&raw); err != nil {
			return 0, nil, nil, err
		}
		return workspaceListDefaultLimit, nil, &raw, nil
	}
	limit := workspaceListDefaultLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > int64(workspaceListMaxLimit) {
			return 0, nil, nil, errors.New("limit must be an integer in [1,100]")
		}
		limit = int32(parsed)
	}
	if raw := values.Get("cursor"); raw != "" {
		cursor, err := decodeWorkspaceListCursor(raw)
		if err != nil {
			return 0, nil, nil, err
		}
		if cursor.ProjectID != projectID || cursor.EnvironmentID != environmentID {
			return 0, nil, nil, errors.New("workspace cursor does not match request scope")
		}
		return limit, &cursor, nil, nil
	}
	return limit, nil, nil, nil
}

func encodeWorkspaceListCursor(cursor workspaceListCursor) (string, error) {
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return workspaceListCursorPrefix + base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeWorkspaceListCursor(raw string) (workspaceListCursor, error) {
	if len(raw) <= len(workspaceListCursorPrefix) || raw[:len(workspaceListCursorPrefix)] != workspaceListCursorPrefix {
		return workspaceListCursor{}, errors.New("workspace cursor is invalid")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(raw[len(workspaceListCursorPrefix):])
	if err != nil {
		return workspaceListCursor{}, errors.New("workspace cursor is invalid")
	}
	var cursor workspaceListCursor
	if err := json.Unmarshal(encoded, &cursor); err != nil {
		return workspaceListCursor{}, errors.New("workspace cursor is invalid")
	}
	if cursor.ProjectID == "" || cursor.EnvironmentID == "" || cursor.CreatedAt.IsZero() || ids.Validate(cursor.ID) != nil {
		return workspaceListCursor{}, errors.New("workspace cursor is invalid")
	}
	return cursor, nil
}
