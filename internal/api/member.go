package api

import "time"

type MemberStatus string

const (
	MemberStatusActive   MemberStatus = "active"
	MemberStatusDisabled MemberStatus = "disabled"
)

type MemberSummary struct {
	UserID      string       `json:"user_id"`
	DisplayName string       `json:"display_name"`
	Email       string       `json:"email,omitempty"`
	Role        string       `json:"role"`
	Status      MemberStatus `json:"status"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	DisabledAt  *time.Time   `json:"disabled_at,omitempty"`
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
	NextCursor  string              `json:"next_cursor,omitempty"`
}

// CreateInvitationsRequest invites up to 50 email addresses with one role and
// expiry.
type CreateInvitationsRequest struct {
	Emails        []string `json:"emails"`
	Role          string   `json:"role"`
	ExpiresInDays *int     `json:"expires_in_days,omitempty"`
}

type InvitationResultStatus string

const (
	InvitationResultInvited InvitationResultStatus = "invited"
	InvitationResultFailed  InvitationResultStatus = "failed"
)

// InvitationResult is the outcome for one distinct address of the request:
// invited, meaning the invitation was emailed, or failed with its error.
type InvitationResult struct {
	Email  string                 `json:"email"`
	Status InvitationResultStatus `json:"status"`
	Error  *HTTPError             `json:"error,omitempty"`
}

// CreateInvitationsResponse lists one result per distinct requested address,
// in request order.
type CreateInvitationsResponse struct {
	Results []InvitationResult `json:"results"`
}
