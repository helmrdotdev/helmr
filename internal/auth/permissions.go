package auth

import (
	"strings"
	"uuid"
)

type Permission string

// PermissionGrant is a request snapshot. Durable owners reauthorize current
// authority at admission; this is not an effective-access cache.
type PermissionGrant struct {
	AssignmentID       string `json:"assignmentId"`
	AssignmentRevision int64  `json:"assignmentRevision"`
	RoleID             string `json:"roleId"`
	RoleRevision       int64  `json:"roleRevision"`
	GroupID            string `json:"groupId,omitempty"`
	Subject            string `json:"subject"`

	ProjectID     string     `json:"projectId"`
	EnvironmentID string     `json:"environmentId"`
	AgentID       string     `json:"agentId,omitempty"`
	Permission    Permission `json:"permission"`
}

func (g PermissionGrant) allows(operation Permission, scope Scope) bool {
	if g.Permission != operation {
		return false
	}
	if !IsPermission(operation) {
		return false
	}
	if g.ProjectID == "" && g.EnvironmentID == "" && g.AgentID == "" {
		if !SupportsScope(operation, ScopeOrganization) {
			return false
		}
		if scope.ProjectID == "" && scope.EnvironmentID == "" && scope.AgentID == "" {
			return true
		}
		return SupportsScope(operation, ScopeEnvironment) && scope.ProjectID != "" && scope.EnvironmentID != ""
	}
	if scope.ProjectID == "" || scope.EnvironmentID == "" || g.ProjectID != scope.ProjectID || g.EnvironmentID != scope.EnvironmentID {
		return false
	}
	if SupportsScope(operation, ScopeEnvironment) {
		return g.AgentID == ""
	}
	return SupportsScope(operation, ScopeAgent) && (g.AgentID == "" || scope.AgentID != "" && g.AgentID == scope.AgentID)

}

type Scope struct {
	OrgID         uuid.UUID `json:"org_id"`
	AgentID       string    `json:"agent_id,omitempty"`
	ProjectID     string    `json:"project_id,omitempty"`
	EnvironmentID string    `json:"environment_id,omitempty"`
}

// SessionExecutionOperation identifies actions that may execute new Agent work.
// Management alone can observe and stop work, but cannot submit or resume it.
func SessionExecutionOperation(permission Permission) bool {
	switch permission {
	case PermissionSessionsSend, PermissionSessionsAnswer, PermissionSessionsResume:
		return true
	}
	return false
}

// PermissionDecision is the sufficient path actually used for one permission.
// It contains references and revisions, not a copy of the current policy graph.
type PermissionDecision struct {
	Permission         Permission `json:"permission"`
	Scope              Scope      `json:"scope"`
	AssignmentID       string     `json:"assignment_id"`
	AssignmentRevision int64      `json:"assignment_revision"`
	RoleID             string     `json:"role_id"`
	RoleRevision       int64      `json:"role_revision"`
	MemberID           uuid.UUID  `json:"member_id,omitempty"`
	GroupID            string     `json:"group_id,omitempty"`
	Subject            string     `json:"subject"`
}

func (a Principal) HasPermission(permission Permission, scope Scope) bool {
	_, ok := a.Decide(permission, scope)
	return ok
}

// PermissionEvaluation preserves the actual evaluated scope and refusal reason.
// A denied check has no granting path; it does not inventory rejected grants.
type PermissionEvaluation struct {
	SubjectUserID   uuid.UUID           `json:"subject_user_id,omitzero"`
	SubjectAPIKeyID uuid.UUID           `json:"subject_api_key_id,omitzero"`
	Permission      Permission          `json:"permission"`
	Scope           Scope               `json:"scope"`
	Result          string              `json:"result"`
	Reason          string              `json:"reason,omitempty"`
	Evidence        *PermissionDecision `json:"evidence,omitempty"`
}

func (a Principal) Evaluate(permission Permission, scope Scope) ([]PermissionEvaluation, bool) {
	check := PermissionEvaluation{SubjectUserID: a.UserID, SubjectAPIKeyID: a.APIKeyID, Permission: permission, Scope: scope, Result: "failed"}
	refuse := func(reason string) ([]PermissionEvaluation, bool) {
		check.Reason = reason
		return []PermissionEvaluation{check}, false
	}
	if !IsPermission(permission) {
		return refuse("unsupported_permission")
	}
	if scope.OrgID != uuid.Nil() && a.OrgID != uuid.Nil() && scope.OrgID != a.OrgID {
		return refuse("organization_mismatch")
	}
	if a.Kind == PrincipalKindAPIKey {
		if !IsMachinePermission(permission) {
			return refuse("key_permission_not_supported")
		}
		if !a.matchesEnvironmentScope(scope) {
			return refuse("key_scope_mismatch")
		}
	}
	for _, grant := range a.Grants {
		if grant.allows(permission, scope) {
			check.Result = "passed"
			check.Evidence = &PermissionDecision{Permission: permission, Scope: scope, AssignmentID: grant.AssignmentID, AssignmentRevision: grant.AssignmentRevision, RoleID: grant.RoleID, RoleRevision: grant.RoleRevision, MemberID: a.MemberID, GroupID: grant.GroupID, Subject: grant.Subject}
			return []PermissionEvaluation{check}, true
		}
	}
	check.Reason = "no_matching_grant"
	return []PermissionEvaluation{check}, false
}

func (a Principal) Decide(permission Permission, scope Scope) ([]PermissionDecision, bool) {
	checks, ok := a.Evaluate(permission, scope)
	if !ok {
		return nil, false
	}
	decisions := make([]PermissionDecision, 0, len(checks))
	for _, check := range checks {
		decisions = append(decisions, *check.Evidence)
	}
	return decisions, true
}

func (a Principal) matchesEnvironmentScope(scope Scope) bool {
	if strings.TrimSpace(scope.ProjectID) == "" || strings.TrimSpace(scope.EnvironmentID) == "" {
		return false
	}
	return strings.TrimSpace(a.ProjectID) == strings.TrimSpace(scope.ProjectID) &&
		strings.TrimSpace(a.EnvironmentID) == strings.TrimSpace(scope.EnvironmentID)
}

// AgentPermissionScope returns current Agent targets for an operation. It does
// not turn selected targets into environment-wide authority.
func (a Principal) AgentPermissionScope(permission Permission, scope Scope) (bool, []uuid.UUID) {
	scope.AgentID = ""
	all := a.HasPermission(permission, scope)
	ids := []uuid.UUID{}
	seen := map[uuid.UUID]bool{}
	for _, grant := range a.Grants {
		if grant.AgentID == "" {
			continue
		}
		id, err := uuid.Parse(grant.AgentID)
		if err != nil || seen[id] {
			continue
		}
		scope.AgentID = grant.AgentID
		if a.HasPermission(permission, scope) {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return all, ids
}

// AuditScope returns current readable audit targets after the same checks used
// for a single target. Candidate grant rows alone are never authority.
func (a Principal) AuditScope() (bool, []uuid.UUID) {
	ids := []uuid.UUID{}
	if a.OrgID == uuid.Nil() {
		return false, ids
	}
	if a.HasPermission(PermissionAuditEventsRead, Scope{OrgID: a.OrgID}) {
		return true, ids
	}
	seen := map[uuid.UUID]bool{}
	for _, g := range a.Grants {
		id, err := uuid.Parse(g.EnvironmentID)
		if err != nil || id == uuid.Nil() || seen[id] {
			continue
		}
		scope := Scope{OrgID: a.OrgID, ProjectID: g.ProjectID, EnvironmentID: g.EnvironmentID}
		if a.HasPermission(PermissionAuditEventsRead, scope) {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return false, ids
}
