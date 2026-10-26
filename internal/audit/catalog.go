package audit

import (
	"slices"
	"strings"
)

// Definition is the capture contract for one product operation. Adding an action
// requires an explicit target and outcomes here, plus an owner and capture test.
type Definition struct {
	Action     string   `json:"action"`
	TargetKind string   `json:"target_kind"`
	Outcomes   []string `json:"outcomes"`
}

var definitions = []Definition{
	{"trigger.content_read", "trigger", []string{"succeeded", "denied", "failed"}},
	{"session.delivery_check_requested", "session", []string{"succeeded", "denied", "failed"}},
	{"session.delivery_abandoned", "session", []string{"succeeded", "denied", "failed"}},
	{"region.created", "region", []string{"succeeded", "denied", "failed"}},
	{"region.updated", "region", []string{"succeeded", "denied", "failed"}},
	{"worker_group.updated", "worker_group", []string{"succeeded", "denied", "failed"}},
	{"template.refreshed", "template", []string{"accepted", "succeeded", "denied", "failed"}},
	{"organization.created", "organization", []string{"succeeded", "denied", "failed"}},
	{"member.granted", "member", []string{"succeeded", "denied", "failed"}},
	{"member.removed", "member", []string{"succeeded", "denied", "failed"}},
	{"member.ownership_changed", "member", []string{"succeeded", "denied", "failed"}},
	{"invitation.created", "invitation", []string{"succeeded", "denied", "failed"}},
	{"invitation.accepted", "invitation", []string{"succeeded", "denied", "failed"}},
	{"invitation.revoked", "invitation", []string{"succeeded", "denied", "failed"}},
	{"invitation.expired", "invitation", []string{"succeeded", "denied", "failed"}},
	{"installation_admin.granted", "user", []string{"succeeded", "denied", "failed"}},
	{"api_key.issued", "api_key", []string{"succeeded", "denied", "failed"}},
	{"api_key.updated", "api_key", []string{"succeeded", "denied", "failed"}},
	{"api_key.rotated", "api_key", []string{"succeeded", "denied", "failed"}},
	{"api_key.revoked", "api_key", []string{"succeeded", "denied", "failed"}},
	{"secret.created", "secret", []string{"succeeded", "denied", "failed"}},
	{"secret.rotated", "secret", []string{"succeeded", "denied", "failed"}},
	{"secret.revoked", "secret", []string{"succeeded", "denied", "failed"}},
	{"secret.exposed", "secret", []string{"succeeded", "denied", "failed"}},
	{"permission_group.created", "permission_group", []string{"succeeded", "denied", "failed"}},
	{"permission_group.updated", "permission_group", []string{"succeeded", "denied", "failed"}},
	{"permission_group.deleted", "permission_group", []string{"succeeded", "denied", "failed"}},
	{"permission_group.member_added", "permission_group", []string{"succeeded", "denied", "failed"}},
	{"permission_group.member_removed", "permission_group", []string{"succeeded", "denied", "failed"}},
	{"role.created", "role", []string{"succeeded", "denied", "failed"}},
	{"role.updated", "role", []string{"succeeded", "denied", "failed"}},
	{"role.deleted", "role", []string{"succeeded", "denied", "failed"}},
	{"role.standard_created", "role", []string{"succeeded", "denied", "failed"}},
	{"role_grant.created", "role_grant", []string{"succeeded", "denied", "failed"}},
	{"role_grant.updated", "role_grant", []string{"succeeded", "denied", "failed"}},
	{"role_grant.deleted", "role_grant", []string{"succeeded", "denied", "failed"}},
	{"access.recovered", "role_grant", []string{"succeeded", "denied", "failed"}},
	{"project.created", "project", []string{"succeeded", "denied", "failed"}},
	{"project.updated", "project", []string{"succeeded", "denied", "failed"}},
	{"project.retention_changed", "project", []string{"succeeded", "denied", "failed"}},
	{"environment.created", "environment", []string{"succeeded", "denied", "failed"}},
	{"environment.updated", "environment", []string{"succeeded", "denied", "failed"}},
	{"agent.deleted", "agent", []string{"succeeded", "denied", "failed"}},
	{"agent.restored", "agent", []string{"succeeded", "denied", "failed"}},
	{"deployment.published", "deployment", []string{"succeeded", "denied", "failed"}},
	{"deployment.promoted", "deployment", []string{"succeeded", "denied", "failed"}},
	{"slack_app.credentials_configured", "slack_app_registration", []string{"succeeded", "denied", "failed"}},
	{"slack_connection.authorized", "slack_connection", []string{"succeeded", "denied", "failed"}},
	{"slack_connection.disconnected", "slack_connection", []string{"succeeded", "denied", "failed"}},
	{"slack_installation.authorization_lost", "slack_installation", []string{"succeeded", "denied", "failed"}},
	{"slack_user.linked", "slack_user_link", []string{"succeeded", "denied", "failed"}},
	{"slack_user.unlinked", "slack_user_link", []string{"succeeded", "denied", "failed"}},
	{"worker_group.created", "worker_group", []string{"succeeded", "denied", "failed"}},
	{"worker_group.token_rotated", "worker_group", []string{"succeeded", "denied", "failed"}},
	{"worker_group.active", "worker_group", []string{"succeeded", "denied", "failed"}},
	{"worker_group.paused", "worker_group", []string{"succeeded", "denied", "failed"}},
	{"worker_group.draining", "worker_group", []string{"succeeded", "denied", "failed"}},
	{"worker_group.disabled", "worker_group", []string{"succeeded", "denied", "failed"}},
	{"worker_pool.created", "worker_pool", []string{"succeeded", "denied", "failed"}},
	{"worker_pool.primary_selected", "worker_pool", []string{"succeeded", "denied", "failed"}},
	{"worker_pool.resource_policy_updated", "worker_pool", []string{"succeeded", "denied", "failed"}},
	{"worker_pool.draining", "worker_pool", []string{"succeeded", "denied", "failed"}},
	{"worker_pool.disabled", "worker_pool", []string{"succeeded", "denied", "failed"}},
	{"worker_host.draining", "worker_host", []string{"succeeded", "denied", "failed"}},
	{"worker_host.lost", "worker_host", []string{"succeeded", "denied", "failed"}},
	{"worker_host.provider_absence_confirmed", "worker_host", []string{"succeeded", "denied", "failed"}},
	{"worker_scale_in.completed", "worker_scale_in_operation", []string{"succeeded", "denied", "failed"}},
	{"identity.sign_in", "login_session", []string{"succeeded", "denied", "failed"}},
	{"identity.session_issued", "login_session", []string{"succeeded", "denied", "failed"}},
	{"identity.session_revoked", "login_session", []string{"succeeded", "denied", "failed"}},
	{"identity.device_started", "device_code", []string{"succeeded", "denied", "failed"}},
	{"identity.device_denied", "device_code", []string{"succeeded", "denied", "failed"}},
	{"identity.device_authorized", "device_code", []string{"succeeded", "denied", "failed"}},
	{"identity.magic_link_requested", "login_session", []string{"succeeded", "denied", "failed"}},
	{"audit.searched", "audit", []string{"succeeded", "denied", "failed"}},
	{"audit.retention_configured", "audit", []string{"succeeded", "denied", "failed"}},
	{"audit.purge_completed", "audit", []string{"succeeded", "denied", "failed"}},
	{"audit.restore_recorded", "audit", []string{"succeeded", "denied", "failed"}},
	{"runtime.operation_refused", "runtime_operation", []string{"denied", "failed"}},
	{"session.created", "session", []string{"succeeded", "denied", "failed"}},
	{"session.admitted", "session", []string{"accepted", "denied", "failed"}},
	{"session.closed", "session", []string{"succeeded", "denied", "failed"}},
	{"session.cancelled", "session", []string{"cancelled", "denied", "failed"}},
	{"session.resumed", "session", []string{"succeeded", "denied", "failed"}},
	{"session.interrupted", "session", []string{"succeeded", "denied", "failed"}},
	{"session.authority_changed", "session", []string{"succeeded", "denied", "failed"}},
	{"session.content_read", "session", []string{"succeeded", "denied", "failed"}},
	{"message.delivered", "message", []string{"succeeded"}},
	{"message.rejected", "message", []string{"succeeded"}},
	{"message.admitted", "message", []string{"accepted", "denied", "failed"}},
	{"session.control_requested", "session", []string{"accepted", "denied", "failed"}},
	{"turn.admitted", "turn", []string{"accepted", "denied", "failed"}},
	{"turn.interrupted", "turn", []string{"succeeded", "denied", "failed"}},
	{"turn.completed", "turn", []string{"succeeded", "denied", "failed"}},
	{"turn.failed", "turn", []string{"failed", "unknown"}},
	{"turn.cancelled", "turn", []string{"cancelled", "denied", "failed"}},
	{"input.requested", "input_request", []string{"accepted", "denied", "failed"}},
	{"input.answered", "input_request", []string{"succeeded", "denied", "failed"}},
	{"input.rejected", "input_request", []string{"succeeded", "denied", "failed"}},
	{"command.cancel_requested", "command", []string{"accepted", "denied", "failed"}},
	{"command.admitted", "command", []string{"accepted", "denied", "failed"}},
	{"command.completed", "command", []string{"succeeded", "denied", "failed"}},
	{"command.failed", "command", []string{"failed", "unknown"}},
	{"command.cancelled", "command", []string{"cancelled", "denied", "failed"}},
	{"command.output_read", "command", []string{"succeeded", "denied", "failed"}},
	{"mcp.admitted", "mcp", []string{"accepted", "denied", "failed"}},
	{"mcp.refused", "mcp", []string{"denied", "failed"}},
	{"computer.control_settled", "computer", []string{"succeeded", "failed"}},
	{"computer.control_requested", "computer", []string{"accepted", "denied", "failed"}},
	{"computer.created", "computer", []string{"succeeded", "denied", "failed"}},
	{"computer.deleted", "computer", []string{"succeeded", "denied", "failed"}},
	{"computer.restore_completed", "computer", []string{"succeeded"}},
	{"computer.checkpoint_published", "computer", []string{"succeeded"}},
	{"computer.retention_changed", "computer", []string{"succeeded", "denied", "failed"}},
}

var catalog = func() map[string]Definition {
	result := make(map[string]Definition, len(definitions))
	for _, d := range definitions {
		if _, ok := result[d.Action]; ok {
			panic("duplicate audit action")
		}
		result[d.Action] = d
	}
	return result
}()

func Catalog() []Definition {
	out := make([]Definition, 0, len(definitions))
	for _, d := range definitions {
		d.Outcomes = slices.Clone(d.Outcomes)
		out = append(out, d)
	}
	return out
}

// Categories group actions for people looking for them. Every action belongs to
// exactly one, by the resource its name starts with.
var categoryOf = map[string]string{
	"access": "access", "member": "access", "invitation": "access", "role": "access", "role_grant": "access",
	"permission_group": "access", "installation_admin": "access", "organization": "access",
	"identity": "sign_in",
	"agent":    "agents", "session": "agents", "turn": "agents", "input": "agents", "message": "agents",
	"trigger": "agents", "mcp": "agents", "runtime": "agents",
	"slack_app": "slack", "slack_installation": "slack", "slack_connection": "slack", "slack_user": "slack",
	"computer": "computers", "command": "computers", "template": "computers",
	"secret": "secrets", "api_key": "secrets",
	"deployment": "deployments", "project": "deployments", "environment": "deployments",
	"region": "infrastructure", "worker_group": "infrastructure", "worker_pool": "infrastructure",
	"worker_host": "infrastructure", "worker_scale_in": "infrastructure",
	"audit": "audit",
}

// Category is the group an action belongs to.
func Category(action string) string {
	resource, _, _ := strings.Cut(action, ".")
	return categoryOf[resource]
}

func Lookup(action string) (Definition, bool) {
	d, ok := catalog[action]
	d.Outcomes = slices.Clone(d.Outcomes)
	return d, ok
}
