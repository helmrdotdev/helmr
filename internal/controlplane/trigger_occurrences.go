package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"net/http"
	"strconv"
	"time"
)

type occurrenceCursor struct {
	Project, Environment, Activation string
	After                            time.Time
}

func (s *Server) listTriggerOccurrences(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	scope, project, env, err := s.requestEnvironmentScopeFromRequest(r, p)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	id, err := ids.Parse(chi.URLParam(r, "triggerID"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	all, agents := p.AgentPermissionScope(auth.PermissionSessionsRead, scope)
	authorized := make([]pgtype.UUID, 0, len(agents))
	for _, a := range agents {
		authorized = append(authorized, pgvalue.UUID(a))
	}
	if !all && len(authorized) == 0 {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	if _, err = s.db.GetTriggerByID(r.Context(), db.GetTriggerByIDParams{OrgID: pgvalue.UUID(p.OrgID), ProjectID: project, EnvironmentID: env, ID: pgvalue.UUID(id), AllAgents: all, AuthorizedAgents: authorized}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, notFound(errors.New("trigger not found")))
		} else {
			writeError(w, err)
		}
		return
	}
	q := r.URL.Query()
	for k, v := range q {
		if (k != "limit" && k != "cursor") || len(v) != 1 || v[0] == "" {
			writeError(w, badRequest(errors.New("invalid occurrence query")))
			return
		}
	}
	limit := 50
	if q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > 100 {
			writeError(w, badRequest(errors.New("limit must be between 1 and 100")))
			return
		}
	}
	var after pgtype.Timestamptz
	if q.Has("cursor") {
		c, e := decodeOccurrenceCursor(q.Get("cursor"))
		if e != nil || c.Project != scope.ProjectID || c.Environment != scope.EnvironmentID || c.Activation != id.String() || c.After.IsZero() {
			writeError(w, badRequest(errors.New("invalid occurrence cursor")))
			return
		}
		after = pgvalue.Timestamptz(c.After)
	}
	rows, err := s.db.ListTriggerOccurrences(r.Context(), db.ListTriggerOccurrencesParams{OrgID: pgvalue.UUID(p.OrgID), ProjectID: project, EnvironmentID: env, ActivationID: pgvalue.UUID(id), AllAgents: all, AuthorizedAgents: authorized, AfterAt: after, LimitCount: int32(limit + 1)})
	if err != nil {
		writeError(w, err)
		return
	}
	result := api.TriggerOccurrencesPage{Occurrences: []api.TriggerOccurrence{}}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	for _, row := range rows {
		item := api.TriggerOccurrence{ScheduledAt: row.ScheduledAt.Time, EvaluatedAt: row.EvaluatedAt.Time, Disposition: row.Disposition}
		if row.Reason.Valid {
			item.Reason = &row.Reason.String
		}
		if row.SessionID.Valid {
			v := pgvalue.UUIDString(row.SessionID)
			item.SessionID = &v
		}
		if row.TurnID.Valid {
			v := pgvalue.UUIDString(row.TurnID)
			item.TurnID = &v
		}
		result.Occurrences = append(result.Occurrences, item)
	}
	if more {
		raw, _ := json.Marshal(occurrenceCursor{Project: scope.ProjectID, Environment: scope.EnvironmentID, Activation: id.String(), After: rows[len(rows)-1].ScheduledAt.Time})
		result.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	writeJSON(w, http.StatusOK, result)
}

func decodeOccurrenceCursor(value string) (occurrenceCursor, error) {
	var c occurrenceCursor
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return c, errors.New("invalid occurrence cursor")
	}
	return c, nil
}
