package api

import "time"

type APIKeyStatus string

const (
	APIKeyStatusActive  APIKeyStatus = "active"
	APIKeyStatusExpired APIKeyStatus = "expired"
	APIKeyStatusRevoked APIKeyStatus = "revoked"
)

type APIKeySummary struct {
	CreatedByMemberID *string      `json:"created_by_member_id,omitempty"`
	ID                string       `json:"id"`
	Name              string       `json:"name"`
	KeyPrefix         string       `json:"key_prefix,omitempty"`
	Project           string       `json:"project"`
	Environment       string       `json:"environment"`
	RoleNames         []string     `json:"role_names"`
	Status            APIKeyStatus `json:"status"`
	CreatedAt         time.Time    `json:"created_at,omitzero"`
	LastUsedAt        *time.Time   `json:"last_used_at,omitempty"`
	ExpiresAt         *time.Time   `json:"expires_at,omitempty"`
	RevokedAt         *time.Time   `json:"revoked_at,omitempty"`
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
	Name          string                `json:"name"`
	ExpiresInDays *int                  `json:"expires_in_days"`
	Role          *APIKeyRoleAssignment `json:"role,omitempty"`
}

// APIKeyRoleAssignment uses the key's environment; permissions belong to the Role.
type APIKeyRoleAssignment struct {
	RoleID               string   `json:"role_id"`
	ExpectedRoleRevision int64    `json:"expected_role_revision"`
	AgentsMode           string   `json:"agents_mode"`
	AgentIDs             []string `json:"agent_ids"`
}

type UpdateAPIKeyRequest struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at"`
}
