package api

import (
	"encoding/json"
	"time"
)

type WaitpointTokenAction string

const (
	WaitpointTokenActionApprove  WaitpointTokenAction = "approve"
	WaitpointTokenActionDeny     WaitpointTokenAction = "deny"
	WaitpointTokenActionMessage  WaitpointTokenAction = "message"
	WaitpointTokenActionReply    WaitpointTokenAction = "reply"
	WaitpointTokenActionComplete WaitpointTokenAction = "complete"
)

type CreateWaitpointTokenRequest struct {
	RunID            string                 `json:"run_id"`
	WaitpointID      string                 `json:"waitpoint_id"`
	Actions          []WaitpointTokenAction `json:"actions,omitempty"`
	ExpiresAt        *time.Time             `json:"expires_at,omitempty"`
	ExpiresInSeconds *int32                 `json:"expires_in_seconds,omitempty"`
	Metadata         json.RawMessage        `json:"metadata,omitempty"`
}

type WaitpointTokenResponse struct {
	ID          string     `json:"id"`
	RunID       string     `json:"run_id"`
	WaitpointID string     `json:"waitpoint_id"`
	URL         string     `json:"url"`
	Token       string     `json:"token,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

type CompleteWaitpointTokenRequest struct {
	Token           string               `json:"token,omitempty"`
	Action          WaitpointTokenAction `json:"action"`
	Reason          string               `json:"reason,omitempty"`
	Text            string               `json:"text,omitempty"`
	Value           json.RawMessage      `json:"value,omitempty"`
	ExternalSubject string               `json:"external_subject,omitempty"`
	Metadata        json.RawMessage      `json:"metadata,omitempty"`
	Attachments     []json.RawMessage    `json:"attachments,omitempty"`
}
