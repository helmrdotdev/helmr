package auth

import (
	"slices"
	"strings"

	"github.com/google/uuid"
)

type Permission string

const (
	PermissionAPIKeysManage     Permission = "api_keys.manage"
	PermissionMembersManage     Permission = "members.manage"
	PermissionProjectsManage    Permission = "projects.manage"
	PermissionRunsCreate        Permission = "runs.create"
	PermissionRunsRead          Permission = "runs.read"
	PermissionRunsManage        Permission = "runs.manage"
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
		case PermissionRunsCreate, PermissionRunsRead, PermissionRunsManage, PermissionTasksDeploy, PermissionWaitpointsRespond:
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
		if !sameScopeValue(grant.ProjectID, scope.ProjectID) || !sameScopeValue(grant.EnvironmentID, scope.EnvironmentID) {
			continue
		}
		if slices.Contains(grant.Permissions, permission) {
			return true
		}
	}
	return false
}

func sameScopeValue(grantValue string, scopeValue string) bool {
	grantValue = strings.TrimSpace(grantValue)
	scopeValue = strings.TrimSpace(scopeValue)
	return grantValue == "*" || grantValue == scopeValue
}
