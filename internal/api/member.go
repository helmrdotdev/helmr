package api

import "time"

type MemberSummary struct {
	UserID      string     `json:"user_id"`
	DisplayName string     `json:"display_name"`
	Email       string     `json:"email,omitempty"`
	Role        string     `json:"role"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DisabledAt  *time.Time `json:"disabled_at,omitempty"`
}

type ListMembersResponse struct {
	Members []MemberSummary `json:"members"`
}

type UpdateMemberRoleRequest struct {
	Role         string `json:"role"`
	ExpectedRole string `json:"expected_role"`
}

type InvitationStatus string

const (
	InvitationStatusPending  InvitationStatus = "pending"
	InvitationStatusAccepted InvitationStatus = "accepted"
	InvitationStatusRevoked  InvitationStatus = "revoked"
	InvitationStatusExpired  InvitationStatus = "expired"
)

type InvitationSummary struct {
	ID               string           `json:"id"`
	Email            string           `json:"email"`
	Role             string           `json:"role"`
	Status           InvitationStatus `json:"status"`
	InvitedByUserID  string           `json:"invited_by_user_id,omitempty"`
	AcceptedByUserID string           `json:"accepted_by_user_id,omitempty"`
	RevokedByUserID  string           `json:"revoked_by_user_id,omitempty"`
	CreatedAt        time.Time        `json:"created_at"`
	ExpiresAt        time.Time        `json:"expires_at"`
	AcceptedAt       *time.Time       `json:"accepted_at,omitempty"`
	RevokedAt        *time.Time       `json:"revoked_at,omitempty"`
}

type ListInvitationsResponse struct {
	Invitations []InvitationSummary `json:"invitations"`
	HasMore     bool                `json:"has_more"`
}

type CreateInvitationRequest struct {
	Email         string `json:"email"`
	Role          string `json:"role"`
	ExpiresInDays *int   `json:"expires_in_days,omitempty"`
}

type CreateInvitationResponse struct {
	InvitationSummary
	InviteURL string `json:"invite_url"`
}
