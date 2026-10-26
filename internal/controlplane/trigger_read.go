package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
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

const (
	triggerListDefaultLimit = int32(50)
	triggerListMaxLimit     = int32(100)
)

type triggerListCursor struct {
	View          string `json:"view"`
	FilterKey     string `json:"filter_key,omitempty"`
	FilterAgent   string `json:"filter_agent,omitempty"`
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
	AgentID       string `json:"agent_id"`
	TriggerID     string `json:"trigger_id"`
}

func (s *Server) listTriggers(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	limit, cursor, err := parseTriggerListQuery(
		r,
		scope.ProjectID,
		scope.EnvironmentID,
	)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var afterAgent pgtype.UUID
	var afterID pgtype.UUID
	if cursor != nil {
		agent, _ := ids.Parse(cursor.AgentID)
		afterAgent = pgvalue.UUID(agent)
		id, err := ids.Parse(cursor.TriggerID)
		if err != nil {
			writeError(w, badRequest(errors.New("trigger cursor is invalid")))
			return
		}
		afterID = pgvalue.UUID(id)
	}
	rows, err := s.readTriggerContent(r.Context(), principal, scope, int(limit), func(q *db.Queries, allAgents bool, authorizedAgents []pgtype.UUID) ([]db.ListTriggersRow, error) {
		return q.ListTriggers(r.Context(), db.ListTriggersParams{AllAgents: allAgents, AuthorizedAgents: authorizedAgents,
			IncludeHistory: r.URL.Query().Get("view") == "all",
			TriggerKey:     pgtype.Text{String: r.URL.Query().Get("key"), Valid: r.URL.Query().Has("key")},
			OrgID:          pgvalue.UUID(principal.OrgID),
			ProjectID:      projectID,
			EnvironmentID:  environmentID,
			Agent:          pgtype.Text{String: r.URL.Query().Get("agent"), Valid: r.URL.Query().Has("agent")},
			AfterAgentID:   afterAgent,
			AfterID:        afterID,
			LimitCount:     limit + 1,
		})
	})
	if err != nil {
		s.log.Error("list triggers failed", "error", err)
		s.writeOperationError(w, r, err)
		return
	}
	hasMore := len(rows) > int(limit)
	if hasMore {
		rows = rows[:limit]
	}
	response := api.ListTriggersResponse{
		Triggers: make([]api.TriggerResponse, 0, len(rows)),
	}
	for _, row := range rows {
		item, err := triggerResponse(row)
		if err != nil {
			s.log.Error("project trigger failed", "trigger_id", pgvalue.UUIDString(row.ID), "error", err)
			s.writeOperationError(w, r, err)
			return
		}
		response.Triggers = append(response.Triggers, item)
	}
	if hasMore {
		last := rows[len(rows)-1]
		response.NextCursor, err = encodeTriggerListCursor(triggerListCursor{
			View: triggerView(r), FilterKey: r.URL.Query().Get("key"), ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, FilterAgent: r.URL.Query().Get("agent"),
			AgentID: pgvalue.UUIDString(last.AgentID), TriggerID: pgvalue.UUIDString(last.ID),
		})
		if err != nil {
			s.log.Error("encode trigger cursor failed", "error", err)
			s.writeOperationError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func parseTriggerListQuery(
	r *http.Request,
	projectID string,
	environmentID string,
) (int32, *triggerListCursor, error) {
	values := r.URL.Query()
	for name, entries := range values {
		if name != "agent" && name != "key" && name != "view" && name != "cursor" && name != "limit" {
			return 0, nil, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || strings.TrimSpace(entries[0]) == "" {
			return 0, nil, fmt.Errorf("query parameter %q must appear once", name)
		}
	}
	if triggerView(r) != "current" && triggerView(r) != "all" {
		return 0, nil, errors.New("view must be current or all")
	}
	if values.Has("key") && (len(values.Get("key")) > 256 || !utf8.ValidString(values.Get("key")) || strings.ContainsRune(values.Get("key"), 0)) {
		return 0, nil, errors.New("invalid trigger key")
	}
	if values.Has("agent") {
		if err := api.ValidateDefinitionID(values.Get("agent")); err != nil {
			return 0, nil, err
		}
	}
	limit := triggerListDefaultLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > int64(triggerListMaxLimit) {
			return 0, nil, fmt.Errorf(
				"limit must be an integer in [1,%d]",
				triggerListMaxLimit,
			)
		}
		limit = int32(parsed)
	}
	rawCursor := values.Get("cursor")
	if rawCursor == "" {
		return limit, nil, nil
	}
	cursor, err := decodeTriggerListCursor(rawCursor)
	if err != nil {
		return 0, nil, err
	}
	if cursor.ProjectID != projectID || cursor.EnvironmentID != environmentID {
		return 0, nil, errors.New("trigger cursor belongs to another scope")
	}
	if err := ids.Validate(cursor.AgentID); err != nil {
		return 0, nil, errors.New("trigger cursor is invalid")
	}
	if ids.Validate(cursor.TriggerID) != nil {
		return 0, nil, errors.New("trigger cursor is invalid")
	}
	if cursor.FilterAgent != values.Get("agent") || cursor.FilterKey != values.Get("key") || cursor.View != triggerView(r) {
		return 0, nil, errors.New("trigger cursor belongs to another filter")
	}
	return limit, &cursor, nil
}

func encodeTriggerListCursor(cursor triggerListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeTriggerListCursor(raw string) (triggerListCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return triggerListCursor{}, errors.New("trigger cursor is invalid")
	}
	var cursor triggerListCursor
	if json.Unmarshal(decoded, &cursor) != nil ||
		cursor.ProjectID == "" ||
		cursor.EnvironmentID == "" ||
		cursor.AgentID == "" ||
		cursor.TriggerID == "" {
		return triggerListCursor{}, errors.New("trigger cursor is invalid")
	}
	return cursor, nil
}

func (s *Server) getTrigger(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	triggerID, err := ids.Parse(chi.URLParam(r, "triggerID"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	rows, err := s.readTriggerContent(r.Context(), principal, scope, 1, func(q *db.Queries, allAgents bool, authorizedAgents []pgtype.UUID) ([]db.ListTriggersRow, error) {
		row, err := q.GetTriggerByID(r.Context(), db.GetTriggerByIDParams{AllAgents: allAgents, AuthorizedAgents: authorizedAgents,
			OrgID:         pgvalue.UUID(principal.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ID:            pgvalue.UUID(triggerID),
		})
		if err != nil {
			return nil, err
		}
		return []db.ListTriggersRow{db.ListTriggersRow(row)}, nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("trigger not found")))
		return
	}
	if err != nil {
		s.log.Error("get trigger failed", "trigger_id", triggerID.String(), "error", err)
		s.writeOperationError(w, r, err)
		return
	}
	response, err := triggerResponse(rows[0])
	if err != nil {
		s.log.Error("project trigger failed", "trigger_id", triggerID.String(), "error", err)
		s.writeOperationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func triggerResponse(row db.ListTriggersRow) (api.TriggerResponse, error) {
	if !row.ID.Valid || !row.AgentID.Valid || !row.DeploymentID.Valid || !row.ActiveFrom.Valid || !row.NextFireAt.Valid {
		return api.TriggerResponse{}, errors.New("trigger identity or interval is invalid")
	}
	result := api.TriggerResponse{
		Agent:        row.Agent,
		ID:           pgvalue.UUIDString(row.ID),
		DeploymentID: pgvalue.UUIDString(row.DeploymentID), TriggerKey: row.TriggerKey,
		Cron: api.TriggerCron{Pattern: row.Cron, Timezone: row.Timezone}, Input: row.Input,
		ActiveFrom: row.ActiveFrom.Time.UTC(), ActiveUntil: pgvalue.TimePtr(row.ActiveUntil),
	}
	if !row.ActiveUntil.Valid || row.NextFireAt.Time.Before(row.ActiveUntil.Time) {
		result.NextFireAt = pgvalue.TimePtr(row.NextFireAt)
	}
	return result, nil
}

func triggerView(r *http.Request) string {
	if v := r.URL.Query().Get("view"); v != "" {
		return v
	}
	return "current"
}

// Trigger inputs are content. Selection, current permission and disclosure
// capture share one snapshot; callers receive rows only after capture commits.
func (s *Server) readTriggerContent(ctx context.Context, principal auth.Principal, scope auth.Scope, captureLimit int, selectRows func(*db.Queries, bool, []pgtype.UUID) ([]db.ListTriggersRow, error)) ([]db.ListTriggersRow, error) {
	var rows []db.ListTriggersRow
	err := db.RunTx(ctx, s.tx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
			return err
		}
		current, err := access.Current(ctx, tx, principal, scope)
		if errors.Is(err, access.ErrDenied) {
			return forbidden(err)
		}
		if err != nil {
			return err
		}
		all, agents := current.AgentPermissionScope(auth.PermissionTriggersRead, scope)
		if !all && len(agents) == 0 {
			return forbidden(access.ErrDenied)
		}
		authorized := make([]pgtype.UUID, 0, len(agents))
		for _, id := range agents {
			authorized = append(authorized, pgvalue.UUID(id))
		}
		rows, err = selectRows(db.New(tx), all, authorized)
		if err != nil {
			return err
		}
		for _, row := range rows[:min(len(rows), captureLimit)] {
			targetScope := scope
			targetScope.AgentID = pgvalue.UUIDString(row.AgentID)
			actor, err := audit.Authorized(current, auth.PermissionTriggersRead, targetScope)
			if err != nil {
				return err
			}
			if err := audit.Append(ctx, tx, s.auditPolicy, audit.Event{Evidence: actor, OrgID: current.OrgID, ProjectID: uuid.MustParse(scope.ProjectID), EnvironmentID: uuid.MustParse(scope.EnvironmentID), DeploymentID: pgvalue.MustUUIDValue(row.DeploymentID), Action: "trigger.content_read", TargetKind: "trigger", TargetID: pgvalue.MustUUIDValue(row.ID), Details: audit.LifecycleDetails{}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}
