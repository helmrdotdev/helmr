package controlplane

import (
	"errors"
	auditquery "github.com/helmrdotdev/helmr/internal/audit/query"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/ids"
)

func auditRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip, _ := netip.ParseAddr(host)
		ctx := audit.WithRequest(r.Context(), audit.Request{OperationID: uuid.NewV7(), SourceIP: ip, Channel: "http"})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func (s *Server) listAuditEvents(w http.ResponseWriter, r *http.Request) {
	s.searchAuditEvents(w, r, false)
}
func (s *Server) adminListAuditEvents(w http.ResponseWriter, r *http.Request) {
	s.searchAuditEvents(w, r, true)
}
func (s *Server) searchAuditEvents(w http.ResponseWriter, r *http.Request, installation bool) {
	f, err := parseAuditFilter(r)
	if err != nil {
		s.writeOperationError(w, r, badRequest(err))
		return
	}
	principal := principalFromContext(r.Context())
	var result auditquery.SearchResult
	if installation {
		result, err = auditquery.SearchInstallation(r.Context(), s.tx, s.auditPolicy, principal, f)
	} else {
		result, err = auditquery.Search(r.Context(), s.tx, s.auditPolicy, principal, f)
	}
	if errors.Is(err, auditquery.ErrSearchDenied) {
		s.writeOperationError(w, r, forbidden(err))
		return
	}
	if errors.Is(err, auditquery.ErrSearchInput) {
		s.writeOperationError(w, r, badRequest(err))
		return
	}
	if err != nil {
		s.writeOperationError(w, r, unavailable(errors.New("audit search is unavailable")))
		return
	}
	response := api.AuditEventsResponse{Events: make([]api.AuditEvent, 0, len(result.Events))}
	for _, record := range result.Events {
		response.Events = append(response.Events, auditHTTPRecord(record))
	}
	if result.NextBefore != nil {
		response.NextBefore = &api.AuditBefore{RecordedAt: result.NextBefore.RecordedAt, ID: result.NextBefore.ID}
	}
	writeJSON(w, http.StatusOK, response)
}

// listAuditActions returns the actions the audit trail can record, so filters
// offer every one under its category. It is static metadata, not audit data.
func (s *Server) listAuditActions(w http.ResponseWriter, _ *http.Request) {
	response := api.AuditActionsResponse{Actions: []api.AuditAction{}}
	for _, d := range audit.Catalog() {
		response.Actions = append(response.Actions, api.AuditAction{Action: d.Action, TargetKind: d.TargetKind, Outcomes: d.Outcomes, Category: audit.Category(d.Action)})
	}
	writeJSON(w, http.StatusOK, response)
}

func parseAuditFilter(r *http.Request) (auditquery.SearchFilter, error) {
	q := r.URL.Query()
	f := auditquery.SearchFilter{ActorKind: q.Get("actor_kind"), TargetKind: q.Get("target_kind"), Outcome: q.Get("outcome"), SlackTeamID: q.Get("slack_team_id"), SlackUserID: q.Get("slack_user_id")}
	f.Actions = q["action"]
	var err error
	if f.From, err = time.Parse(time.RFC3339Nano, q.Get("from")); err != nil {
		return f, auditquery.ErrSearchInput
	}
	if f.To, err = time.Parse(time.RFC3339Nano, q.Get("to")); err != nil {
		return f, auditquery.ErrSearchInput
	}
	if q.Has("limit") {
		if f.Limit, err = strconv.Atoi(q.Get("limit")); err != nil || f.Limit < 1 {
			return f, auditquery.ErrSearchInput
		}
	}
	for _, field := range []struct {
		name  string
		value *uuid.UUID
	}{{"organization_id", &f.OrgID}, {"environment_id", &f.EnvironmentID}, {"actor_id", &f.ActorID}, {"target_id", &f.TargetID}, {"target_user_id", &f.TargetUserID}, {"operation_id", &f.OperationID}, {"event_id", &f.EventID}} {
		if q.Has(field.name) {
			*field.value, err = ids.Parse(q.Get(field.name))
			if err != nil {
				return f, auditquery.ErrSearchInput
			}
		}
	}
	if q.Has("before_recorded_at") || q.Has("before_id") {
		key := auditquery.Before{}
		key.RecordedAt, err = time.Parse(time.RFC3339Nano, q.Get("before_recorded_at"))
		if err != nil {
			return f, auditquery.ErrSearchInput
		}
		key.ID, err = ids.Parse(q.Get("before_id"))
		if err != nil {
			return f, auditquery.ErrSearchInput
		}
		f.Before = &key
	}
	return f, nil
}

func auditHTTPRecord(r auditquery.Record) api.AuditEvent {
	record := api.AuditEvent{
		ID:               r.ID,
		OrgID:            r.OrgID,
		ProjectID:        r.ProjectID,
		EnvironmentID:    r.EnvironmentID,
		SchemaVersion:    r.SchemaVersion,
		OccurredAt:       r.OccurredAt,
		RecordedAt:       r.RecordedAt,
		RetentionSeconds: r.RetentionSeconds,
		ExpiresAt:        r.ExpiresAt,
		ActorUserID:      r.ActorUserID, ActorAPIKeyID: r.ActorAPIKeyID, ActorSessionID: r.ActorSessionID, ActorWorkerHostID: r.ActorWorkerHostID, ActorComponent: r.ActorComponent,
		RuntimeGeneration: r.RuntimeGeneration, ProcessEpoch: r.ProcessEpoch,
		Authentication:   api.AuditAuthentication{Result: r.AuthenticationResult, Origin: r.AuthenticationOrigin, Provider: r.IdentityProvider, CredentialKind: r.CredentialKind, CredentialID: r.CredentialID, CredentialVersion: r.CredentialVersion},
		Action:           r.Action,
		Outcome:          r.Outcome,
		TargetKind:       r.TargetKind,
		TargetID:         r.TargetID,
		TargetUserID:     r.TargetUserID,
		SlackTeamID:      r.SlackTeamID,
		SlackUserID:      r.SlackUserID,
		TargetResolution: r.TargetResolution,
		Authorization:    r.Authorization,
		OperationID:      r.OperationID,
		SessionID:        r.SessionID,
		TurnID:           r.TurnID,
		ComputerID:       r.ComputerID,
		DeploymentID:     r.DeploymentID,
		CauseEventID:     r.CauseEventID,
		Causality:        r.Causality,
		SourceIP:         r.SourceIP,
		Channel:          r.Channel,
		Details:          r.Details,
	}
	if r.ActorIdentity != nil {
		record.ActorIdentity = &api.AuditIdentity{Source: r.ActorIdentity.Source, DisplayName: r.ActorIdentity.DisplayName, Email: r.ActorIdentity.Email}
	}
	return record
}
