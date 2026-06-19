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
	APIKeyScopeRunsCreate              APIKeyScope = "runs:create"
	APIKeyScopeRunsRead                APIKeyScope = "runs:read"
	APIKeyScopeRunsManage              APIKeyScope = "runs:manage"
	APIKeyScopeRunWaitpointsRead       APIKeyScope = "run-waitpoints:read"
	APIKeyScopeWaitpointTokensCreate   APIKeyScope = "waitpoint-tokens:create"
	APIKeyScopeWaitpointTokensRead     APIKeyScope = "waitpoint-tokens:read"
	APIKeyScopeWaitpointTokensComplete APIKeyScope = "waitpoint-tokens:complete"
	APIKeyScopeChannelsWrite           APIKeyScope = "channels:write"
	APIKeyScopeSecretsWrite            APIKeyScope = "secrets:write"
	APIKeyScopeTasksDeploy             APIKeyScope = "tasks:deploy"
)
