package auth

import (
	"slices"
	"strings"

	"uuid"
)

type Permission string

const (
	PermissionAPIKeysManage         Permission = "api_keys.manage"
	PermissionMembersManage         Permission = "members.manage"
	PermissionProjectsManage        Permission = "projects.manage"
	PermissionAsksRespond           Permission = "asks.respond"
	PermissionSessionsRead          Permission = "sessions.read"
	PermissionAgentsStart           Permission = "agents.start"
	PermissionSessionsSend          Permission = "sessions.send"
	PermissionSessionsClose         Permission = "sessions.close"
	PermissionSessionsCancel        Permission = "sessions.cancel"
	PermissionSessionsInterrupt     Permission = "sessions.interrupt"
	PermissionSessionsResume        Permission = "sessions.resume"
	PermissionComputersCreate       Permission = "computers.create"
	PermissionComputersRead         Permission = "computers.read"
	PermissionComputersDelete       Permission = "computers.delete"
	PermissionComputerCommandCreate Permission = "computer.exec.create"
	PermissionSecretsWrite          Permission = "secrets.write"
	PermissionDeploymentsWrite      Permission = "deployments.write"
)

// AllPermissions lists every Permission in declaration order. Callers that
// advertise or enumerate permissions iterate this list instead of repeating
// the constants.
func AllPermissions() []Permission {
	return []Permission{
		PermissionAsksRespond,
		PermissionAPIKeysManage,
		PermissionMembersManage,
		PermissionProjectsManage,
		PermissionSessionsRead,
		PermissionAgentsStart,
		PermissionSessionsSend,
		PermissionSessionsClose,
		PermissionSessionsCancel,
		PermissionSessionsInterrupt,
		PermissionSessionsResume,
		PermissionComputersCreate,
		PermissionComputersRead,
		PermissionComputersDelete,
		PermissionComputerCommandCreate,
		PermissionSecretsWrite,
		PermissionDeploymentsWrite,
	}
}

type Scope struct {
	OrgID         uuid.UUID
	ProjectID     string
	EnvironmentID string
}

func (a Principal) HasPermission(permission Permission, scope Scope) bool {
	if scope.OrgID != uuid.Nil() && a.OrgID != uuid.Nil() && scope.OrgID != a.OrgID {
		return false
	}
	if a.Kind == PrincipalKindAPIKey {
		return RoleAllows(a.Role, permission) && a.matchesEnvironmentScope(scope) && slices.Contains(a.Permissions, permission)
	}
	return RoleAllows(a.Role, permission)
}

func (a Principal) matchesEnvironmentScope(scope Scope) bool {
	if strings.TrimSpace(scope.ProjectID) == "" || strings.TrimSpace(scope.EnvironmentID) == "" {
		return false
	}
	return strings.TrimSpace(a.ProjectID) == strings.TrimSpace(scope.ProjectID) &&
		strings.TrimSpace(a.EnvironmentID) == strings.TrimSpace(scope.EnvironmentID)
}

func RoleAllows(role Role, permission Permission) bool {
	switch role {
	case RoleOwner, RoleAdmin:
		return true
	case RoleDeveloper:
		switch permission {
		case PermissionSessionsRead,
			PermissionAgentsStart,
			PermissionSessionsSend,
			PermissionSessionsClose,
			PermissionSessionsCancel,
			PermissionSessionsInterrupt,
			PermissionSessionsResume,
			PermissionComputersCreate,
			PermissionComputersRead,
			PermissionComputersDelete,
			PermissionComputerCommandCreate,
			PermissionDeploymentsWrite:
			return true
		default:
			return false
		}
	case RoleViewer:
		switch permission {
		case PermissionSessionsRead,
			PermissionComputersRead:
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func ParseAPIKeyGrant(value string) (Permission, bool) {
	permission := Permission(strings.TrimSpace(value))
	switch permission {
	case PermissionAsksRespond,
		PermissionSessionsRead,
		PermissionAgentsStart,
		PermissionSessionsSend,
		PermissionSessionsClose,
		PermissionSessionsCancel,
		PermissionSessionsInterrupt,
		PermissionSessionsResume,
		PermissionComputersCreate,
		PermissionComputersRead,
		PermissionComputersDelete,
		PermissionComputerCommandCreate,
		PermissionSecretsWrite,
		PermissionDeploymentsWrite:
		return permission, true
	default:
		return "", false
	}
}
