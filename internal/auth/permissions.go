package auth

import (
	"strings"

	"github.com/google/uuid"
)

const (
	DefaultProjectID     = "default"
	DefaultEnvironmentID = "default"
)

type Permission string

const (
	PermissionAPIKeysManage     Permission = "api_keys.manage"
	PermissionMembersManage     Permission = "members.manage"
	PermissionProjectsManage    Permission = "projects.manage"
	PermissionRunsCreate        Permission = "runs.create"
	PermissionRunsRead          Permission = "runs.read"
	PermissionSecretsWrite      Permission = "secrets.write"
	PermissionTasksDeploy       Permission = "tasks.deploy"
	PermissionWaitpointPolicies Permission = "waitpoint_policies.manage"
	PermissionWaitpointsRespond Permission = "waitpoints.respond"
)

type Scope struct {
	OrgID         uuid.UUID
	ProjectID     string
	EnvironmentID string
}

type PermissionGrant struct {
	ProjectID     string
	EnvironmentID string
	Permissions   []Permission
}

func DefaultScope(orgID uuid.UUID) Scope {
	return Scope{
		OrgID:         orgID,
		ProjectID:     DefaultProjectID,
		EnvironmentID: DefaultEnvironmentID,
	}
}

func (a Actor) HasPermission(permission Permission, scope Scope) bool {
	if scope.OrgID != uuid.Nil && a.OrgID != uuid.Nil && scope.OrgID != a.OrgID {
		return false
	}
	if a.Kind == ActorKindAPIKey {
		return RoleAllows(a.Role, permission) && grantsAllow(a.Permissions, permission, scope)
	}
	return RoleAllows(a.Role, permission)
}

func RoleAllows(role Role, permission Permission) bool {
	switch role {
	case RoleOwner, RoleAdmin:
		return true
	case RoleDeveloper:
		switch permission {
		case PermissionRunsCreate, PermissionRunsRead, PermissionTasksDeploy, PermissionWaitpointsRespond:
			return true
		default:
			return false
		}
	case RoleViewer:
		return permission == PermissionRunsRead
	default:
		return false
	}
}

func grantsAllow(grants []PermissionGrant, permission Permission, scope Scope) bool {
	for _, grant := range grants {
		if !sameScopeValue(grant.ProjectID, scope.ProjectID, DefaultProjectID) || !sameScopeValue(grant.EnvironmentID, scope.EnvironmentID, DefaultEnvironmentID) {
			continue
		}
		for _, granted := range grant.Permissions {
			if granted == permission {
				return true
			}
		}
	}
	return false
}

func sameScopeValue(grantValue string, scopeValue string, defaultValue string) bool {
	grantValue = strings.TrimSpace(grantValue)
	scopeValue = strings.TrimSpace(scopeValue)
	if grantValue == "" {
		grantValue = defaultValue
	}
	if scopeValue == "" {
		scopeValue = defaultValue
	}
	return grantValue == "*" || grantValue == scopeValue
}
