package auth

import (
	"slices"
	"strings"

	"github.com/google/uuid"
)

type Permission string

const (
	PermissionAPIKeysManage           Permission = "api_keys.manage"
	PermissionMembersManage           Permission = "members.manage"
	PermissionProjectsManage          Permission = "projects.manage"
	PermissionRunsCreate              Permission = "runs.create"
	PermissionRunsRead                Permission = "runs.read"
	PermissionRunsManage              Permission = "runs.manage"
	PermissionWorkspacesRead          Permission = "workspaces.read"
	PermissionWorkspacesWrite         Permission = "workspaces.write"
	PermissionWorkspacesManage        Permission = "workspaces.manage"
	PermissionRunWaitpointsRead       Permission = "waitpoints.read"
	PermissionWaitpointTokensCreate   Permission = "waitpoint_tokens.create"
	PermissionWaitpointTokensRead     Permission = "waitpoint_tokens.read"
	PermissionWaitpointTokensComplete Permission = "waitpoint_tokens.complete"
	PermissionChannelsWrite           Permission = "channels.write"
	PermissionSecretsWrite            Permission = "secrets.write"
	PermissionTasksDeploy             Permission = "tasks.deploy"
)

type Scope struct {
	OrgID         uuid.UUID
	ProjectID     string
	EnvironmentID string
}

func (a Actor) HasPermission(permission Permission, scope Scope) bool {
	if scope.OrgID != uuid.Nil && a.OrgID != uuid.Nil && scope.OrgID != a.OrgID {
		return false
	}
	if a.Kind == ActorKindAPIKey {
		return RoleAllows(a.Role, permission) && a.matchesEnvironmentScope(scope) && slices.Contains(a.Permissions, permission)
	}
	return RoleAllows(a.Role, permission)
}

func (a Actor) matchesEnvironmentScope(scope Scope) bool {
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
		case PermissionRunsCreate,
			PermissionRunsRead,
			PermissionRunsManage,
			PermissionWorkspacesRead,
			PermissionWorkspacesWrite,
			PermissionWorkspacesManage,
			PermissionRunWaitpointsRead,
			PermissionWaitpointTokensCreate,
			PermissionWaitpointTokensRead,
			PermissionWaitpointTokensComplete,
			PermissionChannelsWrite,
			PermissionTasksDeploy:
			return true
		default:
			return false
		}
	case RoleViewer:
		switch permission {
		case PermissionRunsRead, PermissionWorkspacesRead, PermissionRunWaitpointsRead, PermissionWaitpointTokensRead:
			return true
		default:
			return false
		}
	default:
		return false
	}
}
