package api

import "time"

type APIKeyStatus string

const (
	APIKeyStatusActive  APIKeyStatus = "active"
	APIKeyStatusExpired APIKeyStatus = "expired"
	APIKeyStatusRevoked APIKeyStatus = "revoked"
)

type APIKeySummary struct {
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	KeyPrefix     string                  `json:"key_prefix"`
	ProjectID     string                  `json:"project_id"`
	EnvironmentID string                  `json:"environment_id"`
	Permissions   []APIKeyPermissionGrant `json:"permissions,omitempty"`
	Status        APIKeyStatus            `json:"status"`
	CreatedAt     time.Time               `json:"created_at"`
	LastUsedAt    *time.Time              `json:"last_used_at"`
	ExpiresAt     *time.Time              `json:"expires_at"`
	RevokedAt     *time.Time              `json:"revoked_at"`
}

type APIKeyIssued struct {
	APIKeySummary
	RawKey string `json:"raw_key"`
}

type ListAPIKeysResponse struct {
	Items   []APIKeySummary `json:"items"`
	HasMore bool            `json:"has_more"`
}

type IssueAPIKeyRequest struct {
	Name          string                  `json:"name"`
	ExpiresInDays *int                    `json:"expires_in_days"`
	Permissions   []APIKeyPermissionGrant `json:"permissions"`
}

type APIKeyPermissionGrant struct {
	Scopes []APIKeyScope `json:"scopes"`
}

type APIKeyScope string

const (
	APIKeyScopeRunsCreate               APIKeyScope = "runs:create"
	APIKeyScopeRunsRead                 APIKeyScope = "runs:read"
	APIKeyScopeRunsManage               APIKeyScope = "runs:manage"
	APIKeyScopeSessionStreamsRead       APIKeyScope = "session-streams:read"
	APIKeyScopeSessionInputSend         APIKeyScope = "session-input:send"
	APIKeyScopeSessionOutputAppend      APIKeyScope = "session-output:append"
	APIKeyScopeTokensCreate             APIKeyScope = "tokens:create"
	APIKeyScopeTokensRead               APIKeyScope = "tokens:read"
	APIKeyScopeTokensComplete           APIKeyScope = "tokens:complete"
	APIKeyScopeTokensCancel             APIKeyScope = "tokens:cancel"
	APIKeyScopeWorkspaceLifecycleManage APIKeyScope = "workspace-lifecycle:manage"
	APIKeyScopeWorkspaceFilesRead       APIKeyScope = "workspace-files:read"
	APIKeyScopeWorkspaceFilesWrite      APIKeyScope = "workspace-files:write"
	APIKeyScopeWorkspaceVersionsRead    APIKeyScope = "workspace-versions:read"
	APIKeyScopeWorkspaceVersionsCapture APIKeyScope = "workspace-versions:capture"
	APIKeyScopeWorkspaceVersionsRestore APIKeyScope = "workspace-versions:restore"
	APIKeyScopeWorkspaceVersionsDiff    APIKeyScope = "workspace-versions:diff"
	APIKeyScopeWorkspaceExecCreate      APIKeyScope = "workspace-exec:create"
	APIKeyScopeWorkspaceExecRead        APIKeyScope = "workspace-exec:read"
	APIKeyScopeWorkspaceExecManage      APIKeyScope = "workspace-exec:manage"
	APIKeyScopeWorkspacePtyCreate       APIKeyScope = "workspace-pty:create"
	APIKeyScopeWorkspacePtyRead         APIKeyScope = "workspace-pty:read"
	APIKeyScopeWorkspacePtyManage       APIKeyScope = "workspace-pty:manage"
	APIKeyScopeSecretsWrite             APIKeyScope = "secrets:write"
	APIKeyScopeTasksDeploy              APIKeyScope = "tasks:deploy"
)
