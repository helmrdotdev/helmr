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
	APIKeys    []APIKeySummary `json:"api_keys"`
	NextCursor string          `json:"next_cursor,omitempty"`
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
	APIKeyScopeAsksRespond           APIKeyScope = "asks:respond"
	APIKeyScopeSessionsRead          APIKeyScope = "sessions:read"
	APIKeyScopeAgentsStart           APIKeyScope = "agents:start"
	APIKeyScopeSessionsSend          APIKeyScope = "sessions:send"
	APIKeyScopeSessionsInterrupt     APIKeyScope = "sessions:interrupt"
	APIKeyScopeSessionsResume        APIKeyScope = "sessions:resume"
	APIKeyScopeSessionsClose         APIKeyScope = "sessions:close"
	APIKeyScopeSessionsCancel        APIKeyScope = "sessions:cancel"
	APIKeyScopeComputersCreate       APIKeyScope = "computers:create"
	APIKeyScopeComputersRead         APIKeyScope = "computers:read"
	APIKeyScopeComputersDelete       APIKeyScope = "computers:delete"
	APIKeyScopeComputerCommandCreate APIKeyScope = "computer-exec:create"
	APIKeyScopeSecretsWrite          APIKeyScope = "secrets:write"
	APIKeyScopeDeploymentsWrite      APIKeyScope = "deployments:write"
)
