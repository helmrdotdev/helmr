package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type computerMembersCursor struct {
	EnvironmentID string    `json:"environment_id"`
	ComputerID    string    `json:"computer_id"`
	Kind          string    `json:"kind"`
	ID            string    `json:"id"`
	CreatedAt     time.Time `json:"created_at"`
}

func computerMembersParams(environmentID, computerID pgtype.UUID, query api.ComputerMembersQuery) (db.ListComputerMembersParams, error) {
	limit := query.Limit
	if limit == 0 {
		limit = computerListDefaultLimit
	}
	if limit < 1 || limit > computerListMaxLimit {
		return db.ListComputerMembersParams{}, errors.New("limit must be an integer in [1,100]")
	}
	params := db.ListComputerMembersParams{EnvironmentID: environmentID, ComputerID: computerID, RowLimit: limit + 1}
	if query.Cursor == "" {
		return params, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(query.Cursor)
	var cursor computerMembersCursor
	if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.CreatedAt.IsZero() || ids.Validate(cursor.ID) != nil ||
		(cursor.Kind != "session" && cursor.Kind != "task" && cursor.Kind != "command") ||
		cursor.EnvironmentID != pgvalue.UUIDString(environmentID) || cursor.ComputerID != pgvalue.UUIDString(computerID) {
		return db.ListComputerMembersParams{}, errors.New("computer member cursor is invalid for this Computer")
	}
	params.HasAfter = true
	params.AfterCreatedAt = pgtype.Timestamptz{Time: cursor.CreatedAt, Valid: true}
	params.AfterID = pgvalue.UUID(uuid.MustParse(cursor.ID))
	params.AfterKind = cursor.Kind
	return params, nil
}

func listComputerMembers(ctx context.Context, queries db.Querier, params db.ListComputerMembersParams) (api.ListComputerMembersResponse, error) {
	rows, err := queries.ListComputerMembers(ctx, params)
	if err != nil {
		return api.ListComputerMembersResponse{}, err
	}
	response := api.ListComputerMembersResponse{Members: make([]api.ComputerMember, 0, len(rows))}
	limit := int(params.RowLimit - 1)
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		cursor, err := json.Marshal(computerMembersCursor{
			EnvironmentID: pgvalue.UUIDString(params.EnvironmentID), ComputerID: pgvalue.UUIDString(params.ComputerID),
			Kind: last.Kind, ID: pgvalue.UUIDString(last.ID), CreatedAt: pgvalue.Time(last.CreatedAt),
		})
		if err != nil {
			return api.ListComputerMembersResponse{}, err
		}
		response.NextCursor = base64.RawURLEncoding.EncodeToString(cursor)
	}
	for _, row := range rows {
		response.Members = append(response.Members, api.ComputerMember{
			Kind: row.Kind, ID: pgvalue.UUIDString(row.ID), RunID: pgvalue.UUIDString(row.RunID), State: row.State, CreatedAt: pgvalue.Time(row.CreatedAt),
		})
	}
	return response, nil
}

func (s *Server) listComputerMembersHTTP(w http.ResponseWriter, r *http.Request) {
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionComputersRead, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	computerID, err := ids.Parse(chi.URLParam(r, "computerID"))
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: "computer ID is invalid"}))
		return
	}
	query := api.ComputerMembersQuery{}
	for name, values := range r.URL.Query() {
		if len(values) != 1 || values[0] == "" {
			err = fmt.Errorf("%s must appear once with a value", name)
			break
		}
		switch name {
		case "cursor":
			query.Cursor = values[0]
		case "limit":
			limit, parseErr := strconv.ParseInt(values[0], 10, 32)
			if parseErr != nil || limit < 1 || limit > int64(computerListMaxLimit) {
				err = errors.New("limit must be an integer in [1,100]")
			}
			query.Limit = int32(limit)
		default:
			err = fmt.Errorf("query parameter %q is not supported", name)
		}
		if err != nil {
			break
		}
	}
	var params db.ListComputerMembersParams
	if err == nil {
		params, err = computerMembersParams(environmentID, pgvalue.UUID(computerID), query)
	}
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	_, err = s.resolveComputerReference(r.Context(), computerReference{
		OrgID: principal.OrgID, ProjectID: projectID, EnvironmentID: environmentID, ID: computerID.String(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "computer_not_found", message: "computer was not found"}))
		return
	}
	var response api.ListComputerMembersResponse
	if err == nil {
		response, err = listComputerMembers(r.Context(), s.db, params)
	}
	if err != nil {
		writeError(w, unavailable(codedError{code: "computer_authority_unavailable", message: "computer authority is unavailable", retryable: true}))
		return
	}
	writeJSON(w, http.StatusOK, response)
}
