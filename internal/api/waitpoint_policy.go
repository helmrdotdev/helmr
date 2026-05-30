package api

import (
	"encoding/json"
	"time"
)

type WaitpointPolicyConfig struct {
	Reviewers  []WaitpointPolicyRule     `json:"reviewers,omitempty"`
	Deliveries []WaitpointPolicyDelivery `json:"deliveries,omitempty"`
	OnTimeout  *WaitpointPolicyTimeout   `json:"on_timeout,omitempty"`
}

type WaitpointPolicyRule struct {
	Type    string `json:"type"`
	Address string `json:"address,omitempty"`
	Role    string `json:"role,omitempty"`
}

type WaitpointPolicyDelivery struct {
	Type string   `json:"type"`
	To   []string `json:"to,omitempty"`
}

type WaitpointPolicyTimeout struct {
	Type string `json:"type"`
}

type WaitpointPolicyResponse struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Label     string          `json:"label"`
	Config    json.RawMessage `json:"config"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type ListWaitpointPoliciesResponse struct {
	Policies []WaitpointPolicyResponse `json:"policies"`
}

type CreateWaitpointPolicyRequest struct {
	Name   string          `json:"name"`
	Label  string          `json:"label,omitempty"`
	Config json.RawMessage `json:"config"`
}

type UpdateWaitpointPolicyRequest struct {
	Label  string          `json:"label,omitempty"`
	Config json.RawMessage `json:"config"`
}

type WaitpointDeliveryResponse struct {
	ID            string     `json:"id"`
	Channel       string     `json:"channel"`
	RecipientKind string     `json:"recipient_kind"`
	Recipient     string     `json:"recipient"`
	Status        string     `json:"status"`
	LastError     *string    `json:"last_error,omitempty"`
	SentAt        *time.Time `json:"sent_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}
