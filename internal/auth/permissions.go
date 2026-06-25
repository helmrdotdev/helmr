package auth

import (
	"slices"
	"strings"

	"github.com/google/uuid"
)

type Permission string

const (
	PermissionAPIKeysManage            Permission = "api_keys.manage"
	PermissionMembersManage            Permission = "members.manage"
	PermissionProjectsManage           Permission = "projects.manage"
	PermissionRunsCreate               Permission = "runs.create"
	PermissionRunsRead                 Permission = "runs.read"
	PermissionRunsManage               Permission = "runs.manage"
	PermissionSessionStreamsRead       Permission = "session.streams.read"
	PermissionSessionInputSend         Permission = "session.input.send"
	PermissionSessionOutputAppend      Permission = "session.output.append"
	PermissionTokensCreate             Permission = "tokens.create"
	PermissionTokensRead               Permission = "tokens.read"
	PermissionTokensComplete           Permission = "tokens.complete"
	PermissionTokensCancel             Permission = "tokens.cancel"
	PermissionWorkspaceLifecycleManage Permission = "workspace.lifecycle.manage"
	PermissionFilesRead                Permission = "workspace.files.read"
	PermissionFilesWrite               Permission = "workspace.files.write"
	PermissionVersionsRead             Permission = "workspace.versions.read"
	PermissionVersionsCapture          Permission = "workspace.versions.capture"
	PermissionVersionsRestore          Permission = "workspace.versions.restore"
	PermissionVersionsDiff             Permission = "workspace.versions.diff"
	PermissionExecCreate               Permission = "workspace.exec.create"
	PermissionExecRead                 Permission = "workspace.exec.read"
	PermissionExecManage               Permission = "workspace.exec.manage"
	PermissionPtyCreate                Permission = "workspace.pty.create"
	PermissionPtyRead                  Permission = "workspace.pty.read"
	PermissionPtyManage                Permission = "workspace.pty.manage"
	PermissionPortsExpose              Permission = "workspace.ports.expose"
	PermissionPortsRead                Permission = "workspace.ports.read"
	PermissionPortsClose               Permission = "workspace.ports.close"
	PermissionSecretsWrite             Permission = "secrets.write"
	PermissionTasksDeploy              Permission = "tasks.deploy"
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
			PermissionSessionStreamsRead,
			PermissionSessionInputSend,
			PermissionSessionOutputAppend,
			PermissionTokensCreate,
			PermissionTokensRead,
			PermissionTokensComplete,
			PermissionTokensCancel,
			PermissionWorkspaceLifecycleManage,
			PermissionFilesRead,
			PermissionFilesWrite,
			PermissionVersionsRead,
			PermissionVersionsCapture,
			PermissionVersionsRestore,
			PermissionVersionsDiff,
			PermissionExecCreate,
			PermissionExecRead,
			PermissionExecManage,
			PermissionPtyCreate,
			PermissionPtyRead,
			PermissionPtyManage,
			PermissionPortsExpose,
			PermissionPortsRead,
			PermissionPortsClose,
			PermissionTasksDeploy:
			return true
		default:
			return false
		}
	case RoleViewer:
		switch permission {
		case PermissionRunsRead,
			PermissionSessionStreamsRead,
			PermissionTokensRead,
			PermissionFilesRead,
			PermissionVersionsRead,
			PermissionExecRead,
			PermissionPtyRead,
			PermissionPortsRead:
			return true
		default:
			return false
		}
	default:
		return false
	}
}
