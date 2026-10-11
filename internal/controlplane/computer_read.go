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
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/ids"
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
	addressed := computerScope(principal.OrgID, projectID, environmentID)
	if exactKey != nil {
		item, err := computer.FindByKey(r.Context(), s.tx, addressed, *exactKey)
		if errors.Is(err, computer.ErrNotFound) {
			writeJSON(w, http.StatusOK, response)
			return
		}
		if err != nil {
			writeError(w, unavailable(codedError{code: "computer_authority_unavailable", message: "computer authority is unavailable", retryable: true}))
			return
		}
		response.Computers = append(response.Computers, apiComputerListItem(item))
		writeJSON(w, http.StatusOK, response)
		return
	}
	page := computer.ListPage{Limit: limit}
	if cursor != nil {
		page.After = &computer.ListPosition{CreatedAt: cursor.CreatedAt, ID: uuid.MustParse(cursor.ID)}
	}
	listing, err := computer.List(r.Context(), s.tx, addressed, page)
	if err != nil {
		writeError(w, unavailable(codedError{code: "computer_authority_unavailable", message: "computer authority is unavailable", retryable: true}))
		return
	}
	for _, item := range listing.Items {
		response.Computers = append(response.Computers, apiComputerListItem(item))
	}
	if listing.More {
		last := listing.Items[len(listing.Items)-1]
		response.NextCursor, err = encodeComputerListCursor(computerListCursor{
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			CreatedAt: last.CreatedAt, ID: last.ID,
		})
		if err != nil {
			writeError(w, errors.New("list Computers"))
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
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
		if err := computer.ValidateKey(&raw); err != nil {
			return 0, nil, nil, err
		}
		return computer.DefaultListLimit, nil, &raw, nil
	}
	limit := computer.DefaultListLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > int64(computer.MaxListLimit) {
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
