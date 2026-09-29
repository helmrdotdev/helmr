package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	computerListDefaultLimit = int32(50)
	computerListMaxLimit     = int32(100)
)

type computerListCursor struct {
	ProjectID     string    `json:"project_id"`
	EnvironmentID string    `json:"environment_id"`
	CreatedAt     time.Time `json:"created_at"`
	ID            string    `json:"id"`
}

func (s *Server) listComputersHTTP(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionComputersRead, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	limit, cursor, exactKey, err := parseComputerListQuery(r, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	response := api.ListComputersResponse{Computers: []api.ComputerListItem{}}
	if exactKey != nil {
		record, err := s.db.GetComputerListItemByKey(r.Context(), db.GetComputerListItemByKeyParams{
			OrgID: pgvalue.UUID(principal.OrgID), ProjectID: projectID,
			EnvironmentID: environmentID, Key: pgvalue.Text(*exactKey),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, response)
			return
		}
		if err != nil {
			writeError(w, unavailable(codedError{code: "computer_authority_unavailable", message: "computer authority is unavailable", retryable: true}))
			return
		}
		item, err := computerListItem(
			record.ID, record.Key, record.SandboxID, record.DeploymentID, record.Status,
			record.LastActivityAt, record.CreatedAt, record.UpdatedAt,
		)
		if err != nil {
			writeError(w, unavailable(codedError{code: "computer_authority_unavailable", message: "computer authority is unavailable", retryable: true}))
			return
		}
		item.Error = record.ResidencyError
		item.Residency = api.ComputerResidency(record.Residency)
		response.Computers = append(response.Computers, item)
		writeJSON(w, http.StatusOK, response)
		return
	}
	params := db.ListComputerListItemsParams{
		OrgID: pgvalue.UUID(principal.OrgID), ProjectID: projectID, EnvironmentID: environmentID,
		RowLimit: limit + 1,
	}
	if cursor != nil {
		params.HasAfter = true
		params.AfterCreatedAt = pgtype.Timestamptz{Time: cursor.CreatedAt, Valid: true}
		params.AfterID = pgvalue.UUID(uuid.MustParse(cursor.ID))
	}
	rows, err := s.db.ListComputerListItems(r.Context(), params)
	if err != nil {
		writeError(w, unavailable(codedError{code: "computer_authority_unavailable", message: "computer authority is unavailable", retryable: true}))
		return
	}
	hasMore := len(rows) > int(limit)
	if hasMore {
		rows = rows[:limit]
	}
	for _, row := range rows {
		item, err := computerListItem(
			row.ID, row.Key, row.SandboxID, row.DeploymentID, row.Status,
			row.LastActivityAt, row.CreatedAt, row.UpdatedAt,
		)
		if err != nil {
			writeError(w, unavailable(codedError{code: "computer_authority_unavailable", message: "computer authority is unavailable", retryable: true}))
			return
		}
		item.Error = row.ResidencyError
		item.Residency = api.ComputerResidency(row.Residency)
		response.Computers = append(response.Computers, item)
	}
	if hasMore {
		last := rows[len(rows)-1]
		response.NextCursor, err = encodeComputerListCursor(computerListCursor{
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			CreatedAt: pgvalue.Time(last.CreatedAt), ID: pgvalue.UUIDString(last.ID),
		})
		if err != nil {
			writeError(w, errors.New("list Computers"))
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func computerListItem(
	id pgtype.UUID,
	keyValue pgtype.Text,
	sandboxID string,
	deploymentID pgtype.UUID,
	state string,
	lastActivityAt, createdAt, updatedAt pgtype.Timestamptz,
) (api.ComputerListItem, error) {
	status, err := computerPublicStatus(state)
	if err != nil {
		return api.ComputerListItem{}, err
	}
	var key *string
	if keyValue.Valid {
		value := keyValue.String
		key = &value
	}
	return api.ComputerListItem{
		ID: pgvalue.UUIDString(id), Key: key, SandboxID: sandboxID,
		DeploymentID: pgvalue.UUIDString(deploymentID), Status: status,
		LastActivityAt: pgvalue.Time(lastActivityAt), CreatedAt: pgvalue.Time(createdAt),
		UpdatedAt: pgvalue.Time(updatedAt),
	}, nil
}

func parseComputerListQuery(
	r *http.Request,
	projectID, environmentID string,
) (int32, *computerListCursor, *string, error) {
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
			return 0, nil, nil, errors.New("computer exact key lookup does not accept cursor or limit")
		}
		if err := validateComputerKey(&raw); err != nil {
			return 0, nil, nil, err
		}
		return computerListDefaultLimit, nil, &raw, nil
	}
	limit := computerListDefaultLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > int64(computerListMaxLimit) {
			return 0, nil, nil, errors.New("limit must be an integer in [1,100]")
		}
		limit = int32(parsed)
	}
	if raw := values.Get("cursor"); raw != "" {
		cursor, err := decodeComputerListCursor(raw)
		if err != nil {
			return 0, nil, nil, err
		}
		if cursor.ProjectID != projectID || cursor.EnvironmentID != environmentID {
			return 0, nil, nil, errors.New("computer cursor does not match request scope")
		}
		return limit, &cursor, nil, nil
	}
	return limit, nil, nil, nil
}

func encodeComputerListCursor(cursor computerListCursor) (string, error) {
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeComputerListCursor(raw string) (computerListCursor, error) {
	encoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return computerListCursor{}, errors.New("computer cursor is invalid")
	}
	var cursor computerListCursor
	if err := json.Unmarshal(encoded, &cursor); err != nil {
		return computerListCursor{}, errors.New("computer cursor is invalid")
	}
	if cursor.ProjectID == "" || cursor.EnvironmentID == "" || cursor.CreatedAt.IsZero() || ids.Validate(cursor.ID) != nil {
		return computerListCursor{}, errors.New("computer cursor is invalid")
	}
	return cursor, nil
}
