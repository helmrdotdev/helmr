package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

type AuditSearchOptions struct {
	From, To                                                                                                                         time.Time
	Before                                                                                                                           *api.AuditBefore
	Limit                                                                                                                            int
	OrganizationID, EnvironmentID, ActorKind, ActorID, Action, TargetKind, TargetID, TargetUserID, SlackTeamID, SlackUserID, Outcome string
}

func (o AuditSearchOptions) values() url.Values {
	q := url.Values{"from": {o.From.Format(time.RFC3339Nano)}, "to": {o.To.Format(time.RFC3339Nano)}}
	if o.Limit != 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	for key, value := range map[string]string{"organization_id": o.OrganizationID, "environment_id": o.EnvironmentID, "actor_kind": o.ActorKind, "actor_id": o.ActorID, "action": o.Action, "target_kind": o.TargetKind, "target_id": o.TargetID, "target_user_id": o.TargetUserID, "slack_team_id": o.SlackTeamID, "slack_user_id": o.SlackUserID, "outcome": o.Outcome} {
		if value != "" {
			q.Set(key, value)
		}
	}
	if o.Before != nil {
		q.Set("before_recorded_at", o.Before.RecordedAt.Format(time.RFC3339Nano))
		q.Set("before_id", o.Before.ID.String())
	}
	return q
}
func (c *Client) ListAuditEvents(ctx context.Context, options AuditSearchOptions) (AuditEventsResponse, error) {
	path := "/v1/audit-events"
	if c.sessionScopedRoutes {
		path = "/api/audit-events"
	}
	return c.auditEvents(ctx, path, options)
}
func (c *Client) ListAdminAuditEvents(ctx context.Context, options AuditSearchOptions) (api.AdminAuditEventsResponse, error) {
	var result api.AdminAuditEventsResponse
	req, err := c.newRequest(ctx, http.MethodGet, "/admin/api/v1/audit-events?"+options.values().Encode(), nil)
	if err != nil {
		return result, err
	}
	err = c.doJSON(req, &result)
	return result, err
}
func (c *Client) auditEvents(ctx context.Context, path string, options AuditSearchOptions) (AuditEventsResponse, error) {
	var result AuditEventsResponse
	req, err := c.newRequest(ctx, http.MethodGet, path+"?"+options.values().Encode(), nil)
	if err != nil {
		return result, err
	}
	err = c.doJSON(req, &result)
	return result, err
}

// AuditEvent is receive-only tenant JSON. Its nested payloads have already passed
// the server projection; this type is never used for server or Admin output.
type AuditEvent struct {
	api.AuditEventMetadata
	Authorization json.RawMessage `json:"authorization"`
	Details       json.RawMessage `json:"details"`
}
type AuditEventsResponse struct {
	Events     []AuditEvent     `json:"events"`
	NextBefore *api.AuditBefore `json:"next_before,omitempty"`
}
