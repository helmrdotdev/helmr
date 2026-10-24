package api

import "time"

type APIKeyStatus string

const (
	APIKeyStatusActive  APIKeyStatus = "active"
	APIKeyStatusExpired APIKeyStatus = "expired"
	APIKeyStatusRevoked APIKeyStatus = "revoked"
)

type APIKeySummary struct {
	CreatedByMemberID *string                 `json:"created_by_member_id"`
	ID                string                  `json:"id"`
	Name              string                  `json:"name"`
	KeyPrefix         string                  `json:"key_prefix"`
	Project           string                  `json:"project"`
	Environment       string                  `json:"environment"`
	Permissions       []APIKeyPermissionGrant `json:"permissions,omitempty"`
	Status            APIKeyStatus            `json:"status"`
	CreatedAt         time.Time               `json:"created_at"`
	LastUsedAt        *time.Time              `json:"last_used_at"`
	ExpiresAt         *time.Time              `json:"expires_at"`
	RevokedAt         *time.Time              `json:"revoked_at"`
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
	APIKeyScopeAgentsUse          APIKeyScope = "agents.use"
	APIKeyScopeSessionsView       APIKeyScope = "sessions.view"
	APIKeyScopeSessionsManage     APIKeyScope = "sessions.manage"
	APIKeyScopeDeploymentsPublish APIKeyScope = "deployments.publish"
	APIKeyScopeDeploymentsPromote APIKeyScope = "deployments.promote"
	APIKeyScopeComputersOperate   APIKeyScope = "computers.operate"
	APIKeyScopeSecretsManage      APIKeyScope = "secrets.manage"
)

type UpdateAPIKeyRequest struct {
	Name        string                  `json:"name"`
	Permissions []APIKeyPermissionGrant `json:"permissions"`
	ExpiresAt   *time.Time              `json:"expires_at"`
}
