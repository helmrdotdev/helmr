package auth

import (
	"slices"
	"strings"

	"uuid"
)

type Permission string

const (
	PermissionAPIKeysManage       Permission = "api_keys.manage"
	PermissionMembersManage       Permission = "members.manage"
	PermissionProjectsManage      Permission = "projects.manage"
	PermissionRunsCreate          Permission = "runs.create"
	PermissionRunsRead            Permission = "runs.read"
	PermissionRunsManage          Permission = "runs.manage"
	PermissionSessionsRead        Permission = "sessions.read"
	PermissionActorsStart         Permission = "actors.start"
	PermissionSessionsInputSend   Permission = "sessions.input.send"
	PermissionSessionsClose       Permission = "sessions.close"
	PermissionTokensCreate        Permission = "tokens.create"
	PermissionTokensRead          Permission = "tokens.read"
	PermissionTokensComplete      Permission = "tokens.complete"
	PermissionTokensCancel        Permission = "tokens.cancel"
	PermissionWorkspacesCreate    Permission = "workspaces.create"
	PermissionWorkspacesRead      Permission = "workspaces.read"
	PermissionWorkspacesDelete    Permission = "workspaces.delete"
	PermissionWorkspaceFilesRead  Permission = "workspace.files.read"
	PermissionWorkspaceExecCreate Permission = "workspace.exec.create"
	PermissionSecretsWrite        Permission = "secrets.write"
	PermissionTasksDeploy         Permission = "tasks.deploy"
)

type Scope struct {
	OrgID         uuid.UUID
	ProjectID     string
	EnvironmentID string
}

func (a Actor) HasPermission(permission Permission, scope Scope) bool {
	if scope.OrgID != uuid.Nil() && a.OrgID != uuid.Nil() && scope.OrgID != a.OrgID {
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
			PermissionSessionsRead,
			PermissionActorsStart,
			PermissionSessionsInputSend,
			PermissionSessionsClose,
			PermissionTokensCreate,
			PermissionTokensRead,
			PermissionTokensComplete,
			PermissionTokensCancel,
			PermissionWorkspacesCreate,
			PermissionWorkspacesRead,
			PermissionWorkspacesDelete,
			PermissionWorkspaceFilesRead,
			PermissionWorkspaceExecCreate,
			PermissionTasksDeploy:
			return true
		default:
			return false
		}
	case RoleViewer:
		switch permission {
		case PermissionRunsRead,
			PermissionSessionsRead,
			PermissionTokensRead,
			PermissionWorkspacesRead,
			PermissionWorkspaceFilesRead:
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
	case PermissionRunsCreate,
		PermissionRunsRead,
		PermissionRunsManage,
		PermissionSessionsRead,
		PermissionActorsStart,
		PermissionSessionsInputSend,
		PermissionSessionsClose,
		PermissionTokensCreate,
		PermissionTokensRead,
		PermissionTokensComplete,
		PermissionTokensCancel,
		PermissionWorkspacesCreate,
		PermissionWorkspacesRead,
		PermissionWorkspacesDelete,
		PermissionWorkspaceFilesRead,
		PermissionWorkspaceExecCreate,
		PermissionSecretsWrite,
		PermissionTasksDeploy:
		return permission, true
	default:
		return "", false
	}
}
