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
	Scope      Scope    `json:"scope"`
	Audience   Audience `json:"audience"`
}

// Scope describes successful custody. Organization actions may also record an
// installation-scoped refusal when no organization has been established.
type Scope string

const (
	OrganizationScope Scope = "organization"
	InstallationScope Scope = "installation"
	EitherScope       Scope = "organization_or_installation"
)

type Audience string

const (
	OrganizationAudience Audience = "organization"
	PlatformAudience     Audience = "platform"
)

var definitions = []Definition{
	{"session.close_requested", "session", []string{"accepted", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"session.cancel_requested", "session", []string{"accepted", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"session.interrupt_requested", "session", []string{"accepted", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"session.resume_requested", "session", []string{"accepted", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"turn.interrupt_requested", "turn", []string{"accepted", "denied", "failed"}, OrganizationScope, OrganizationAudience},

	{"trigger.content_read", "trigger", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"session.delivery_check_requested", "session", []string{"accepted", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"session.delivery_abandoned", "session", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"region.created", "region", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"region.updated", "region", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"worker_group.updated", "worker_group", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"template.refresh_requested", "template", []string{"accepted", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"organization.created", "organization", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"member.granted", "member", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"member.removed", "member", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"member.ownership_changed", "member", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"invitation.created", "invitation", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"invitation.accepted", "invitation", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"invitation.revoked", "invitation", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"invitation.expired", "invitation", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"platform_admin.granted", "user", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"api_key.issued", "api_key", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"api_key.updated", "api_key", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"api_key.rotated", "api_key", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"api_key.revoked", "api_key", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"secret.created", "secret", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"secret.rotated", "secret", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"secret.revoked", "secret", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"secret.exposed", "secret", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"permission_group.created", "permission_group", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"permission_group.updated", "permission_group", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"permission_group.deleted", "permission_group", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"permission_group.member_added", "permission_group", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"permission_group.member_removed", "permission_group", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"role.created", "role", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"role.updated", "role", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"role.deleted", "role", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"role_grant.created", "role_grant", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"role_grant.updated", "role_grant", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"role_grant.deleted", "role_grant", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"access.recovered", "role_grant", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"project.created", "project", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"project.updated", "project", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"project.retention_changed", "project", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"environment.created", "environment", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"environment.updated", "environment", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"agent.deleted", "agent", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"agent.restored", "agent", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"deployment.published", "deployment", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"deployment.promoted", "deployment", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"slack_app.credentials_configured", "slack_app_registration", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"slack_connection.authorized", "slack_connection", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"slack_connection.disconnected", "slack_connection", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"slack_installation.authorization_lost", "slack_installation", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"slack_user.linked", "slack_user_link", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"slack_user.unlinked", "slack_user_link", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"worker_group.created", "worker_group", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"worker_group.token_rotated", "worker_group", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"worker_group.activated", "worker_group", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"worker_group.paused", "worker_group", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"worker_group.drain_started", "worker_group", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"worker_group.disabled", "worker_group", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"worker_pool.created", "worker_pool", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"worker_pool.primary_selected", "worker_pool", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"worker_pool.resource_policy_updated", "worker_pool", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"worker_pool.drain_started", "worker_pool", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"worker_pool.disabled", "worker_pool", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"worker_host.drain_started", "worker_host", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"worker_host.marked_lost", "worker_host", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"worker_host.provider_absence_confirmed", "worker_host", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"worker_scale_in.completed", "worker_scale_in_operation", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"identity.sign_in", "login_session", []string{"succeeded", "denied", "failed"}, EitherScope, OrganizationAudience},
	{"identity.session_revoked", "login_session", []string{"succeeded", "denied", "failed"}, EitherScope, OrganizationAudience},
	{"identity.device_started", "device_code", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"identity.device_denied", "device_code", []string{"succeeded", "denied", "failed"}, EitherScope, OrganizationAudience},
	{"identity.device_authorized", "device_code", []string{"succeeded", "denied", "failed"}, EitherScope, OrganizationAudience},
	{"identity.magic_link_requested", "login_session", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"audit.searched", "audit", []string{"succeeded", "denied", "failed"}, EitherScope, OrganizationAudience},
	{"audit.retention_observed", "audit", []string{"succeeded"}, InstallationScope, PlatformAudience},
	{"audit.purge_completed", "audit", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"audit.restore_recorded", "audit", []string{"succeeded", "denied", "failed"}, InstallationScope, PlatformAudience},
	{"runtime.operation_refused", "runtime_operation", []string{"denied", "failed"}, InstallationScope, PlatformAudience},
	{"session.created", "session", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"session.closed", "session", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"session.cancelled", "session", []string{"cancelled", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"session.hold_released", "session", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"session.hold_created", "session", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"session.content_read", "session", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"message.delivered", "message", []string{"succeeded"}, OrganizationScope, OrganizationAudience},
	{"message.rejected", "message", []string{"succeeded"}, OrganizationScope, OrganizationAudience},
	{"message.admitted", "message", []string{"accepted", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"turn.admitted", "turn", []string{"accepted", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"turn.interrupted", "turn", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"turn.completed", "turn", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"turn.failed", "turn", []string{"failed", "unknown"}, OrganizationScope, OrganizationAudience},
	{"turn.cancelled", "turn", []string{"cancelled", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"input.requested", "input_request", []string{"accepted", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"input.answered", "input_request", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"input.cancelled", "input_request", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"command.cancel_requested", "command", []string{"accepted", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"command.admitted", "command", []string{"accepted", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"command.completed", "command", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"command.failed", "command", []string{"failed", "unknown"}, OrganizationScope, OrganizationAudience},
	{"command.cancelled", "command", []string{"cancelled", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"command.output_read", "command", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"mcp.admitted", "mcp", []string{"accepted", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"mcp.refused", "mcp", []string{"denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"computer.wake_settled", "computer", []string{"succeeded", "failed"}, OrganizationScope, OrganizationAudience},
	{"computer.hibernate_settled", "computer", []string{"succeeded", "failed"}, OrganizationScope, OrganizationAudience},
	{"computer.wake_requested", "computer", []string{"accepted", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"computer.hibernate_requested", "computer", []string{"accepted", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"computer.created", "computer", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"computer.deleted", "computer", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
	{"computer.restore_completed", "computer", []string{"succeeded"}, OrganizationScope, OrganizationAudience},
	{"computer.checkpoint_published", "computer", []string{"succeeded"}, OrganizationScope, OrganizationAudience},
	{"computer.retention_changed", "computer", []string{"succeeded", "denied", "failed"}, OrganizationScope, OrganizationAudience},
}

var catalog = func() map[string]Definition {
	result := make(map[string]Definition, len(definitions))
	for _, d := range definitions {
		if !slices.Contains([]Scope{OrganizationScope, InstallationScope, EitherScope}, d.Scope) || !slices.Contains([]Audience{OrganizationAudience, PlatformAudience}, d.Audience) || (d.Audience == PlatformAudience) != (d.Scope == InstallationScope) {
			panic("unclassified audit action")
		}
		if _, ok := detailDecoder(d.Action); !ok {
			panic("unclassified audit details")
		}
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
	"permission_group": "access", "platform_admin": "access", "organization": "access",
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

// PlatformActions excludes known operator-only facts even if a historical row
// was incorrectly assigned organization custody. Unknown rows still fail projection.
func PlatformActions() []string {
	actions := []string{}
	for _, d := range definitions {
		if d.Audience == PlatformAudience {
			actions = append(actions, d.Action)
		}
	}
	return actions
}
