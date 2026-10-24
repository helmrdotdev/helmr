package auth

import (
	"strings"

	"uuid"
)

type Permission string

const (
	PermissionSecretMetadataRead     Permission = "secret_metadata.read"
	PermissionAgentUse               Permission = "agent.use"
	PermissionRunMetadataRead        Permission = "run_metadata.read"
	PermissionDeploymentMetadataRead Permission = "deployment_metadata.read"
	PermissionDeploymentsPromote     Permission = "deployment.promote"
	PermissionAPIKeysManage          Permission = "api_keys.manage"
	PermissionMembersManage          Permission = "members.manage"
	PermissionProjectsManage         Permission = "projects.manage"
	PermissionAsksAnswer             Permission = "asks.answer"
	PermissionSessionsRead           Permission = "sessions.read"
	PermissionSessionsCreate         Permission = "sessions.create"
	PermissionSessionsSend           Permission = "sessions.send"
	PermissionSessionsClose          Permission = "sessions.close"
	PermissionSessionsCancel         Permission = "sessions.cancel"
	PermissionSessionsInterrupt      Permission = "sessions.interrupt"
	PermissionSessionsResume         Permission = "sessions.resume"
	PermissionComputersCreate        Permission = "computers.create"
	PermissionComputersRead          Permission = "computers.read"
	PermissionComputersDelete        Permission = "computers.delete"
	PermissionComputersUpdate        Permission = "computers.update"
	PermissionComputerCommandCreate  Permission = "computer.exec.create"
	PermissionSecretsWrite           Permission = "secrets.write"
	PermissionDeploymentsWrite       Permission = "deployments.write"
)

// AllPermissions lists every Permission in declaration order. Callers that
// advertise or enumerate permissions iterate this list instead of repeating
// the constants.
func AllPermissions() []Permission {
	return []Permission{
		PermissionAgentUse, PermissionRunMetadataRead, PermissionDeploymentMetadataRead, PermissionDeploymentsPromote, PermissionSecretMetadataRead,
		PermissionAsksAnswer,
		PermissionAPIKeysManage,
		PermissionMembersManage,
		PermissionProjectsManage,
		PermissionSessionsRead,
		PermissionSessionsCreate,
		PermissionSessionsSend,
		PermissionSessionsClose,
		PermissionSessionsCancel,
		PermissionSessionsInterrupt,
		PermissionSessionsResume,
		PermissionComputersCreate,
		PermissionComputersRead,
		PermissionComputersDelete,
		PermissionComputersUpdate,
		PermissionComputerCommandCreate,
		PermissionSecretsWrite,
		PermissionDeploymentsWrite,
	}
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
	case PermissionSessionsSend, PermissionAsksAnswer, PermissionSessionsResume:
		return true
	}
	return false
}

func (a Principal) HasPermission(permission Permission, scope Scope) bool {
	if SessionExecutionOperation(permission) {
		return a.HasPermission(CapabilitySessionsManage, scope) && a.HasPermission(CapabilityAgentsUse, scope)
	}
	if scope.OrgID != uuid.Nil() && a.OrgID != uuid.Nil() && scope.OrgID != a.OrgID {
		return false
	}
	if a.Kind == PrincipalKindAPIKey {
		if !a.matchesEnvironmentScope(scope) {
			return false
		}
		for _, capability := range a.Permissions {
			if IsMachineCapability(capability) && CapabilityAllows(capability, permission) {
				return true
			}
		}
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

func ParseAPIKeyGrant(value string) (Permission, bool) {
	permission := Permission(strings.TrimSpace(value))
	return permission, IsMachineCapability(permission)
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
