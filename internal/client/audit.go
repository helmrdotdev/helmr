package client

import (
	"context"
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
func (c *Client) ListAuditEvents(ctx context.Context, options AuditSearchOptions) (api.AuditEventsResponse, error) {
	path := "/v1/audit-events"
	if c.sessionScopedRoutes {
		path = "/api/audit-events"
	}
	return c.auditEvents(ctx, path, options)
}
func (c *Client) ListInstallationAuditEvents(ctx context.Context, options AuditSearchOptions) (api.AuditEventsResponse, error) {
	return c.auditEvents(ctx, "/admin/api/v1/audit-events", options)
}
func (c *Client) auditEvents(ctx context.Context, path string, options AuditSearchOptions) (api.AuditEventsResponse, error) {
	var result api.AuditEventsResponse
	req, err := c.newRequest(ctx, http.MethodGet, path+"?"+options.values().Encode(), nil)
	if err != nil {
		return result, err
	}
	err = c.doJSON(req, &result)
	return result, err
}
