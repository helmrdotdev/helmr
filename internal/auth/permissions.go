package auth

import (
	"strings"
	"uuid"
)

type Permission string

// PermissionGrant is a request snapshot. Durable owners reauthorize current
// authority at admission; this is not an effective-access cache.
type PermissionGrant struct {
	ProjectID     string     `json:"projectId"`
	EnvironmentID string     `json:"environmentId"`
	AgentID       string     `json:"agentId,omitempty"`
	Permission    Permission `json:"permission"`
}

func (g PermissionGrant) allows(operation Permission, scope Scope) bool {
	if g.Permission != operation {
		return false
	}
	d, ok := PermissionInfo(operation)
	if !ok {
		return false
	}
	if d.ScopeType == ScopeOrganization {
		return g.ProjectID == "" && g.EnvironmentID == "" && g.AgentID == "" && scope.ProjectID == "" && scope.EnvironmentID == "" && scope.AgentID == ""
	}
	if scope.ProjectID == "" || scope.EnvironmentID == "" || g.ProjectID != scope.ProjectID || g.EnvironmentID != scope.EnvironmentID {
		return false
	}
	if d.ScopeType == ScopeEnvironment {
		return g.AgentID == ""
	}
	return g.AgentID == "" || (scope.AgentID != "" && g.AgentID == scope.AgentID)
}

type Scope struct {
	OrgID         uuid.UUID
	AgentID       string
	ProjectID     string
	EnvironmentID string
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

func (a Principal) HasPermission(permission Permission, scope Scope) bool {
	if !IsPermission(permission) {
		return false
	}
	if scope.OrgID != uuid.Nil() && a.OrgID != uuid.Nil() && scope.OrgID != a.OrgID {
		return false
	}
	if a.Kind == PrincipalKindAPIKey && (!IsMachinePermission(permission) || !a.matchesEnvironmentScope(scope)) {
		return false
	}
	if SessionExecutionOperation(permission) && !a.HasPermission(PermissionAgentsUse, scope) {
		return false
	}
	for _, grant := range a.Grants {
		if grant.allows(permission, scope) {
			return true
		}
	}
	return false
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
