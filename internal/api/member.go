package api

import "time"

type MemberStatus string

const (
	MemberStatusSuspended MemberStatus = "suspended"
	MemberStatusActive    MemberStatus = "active"
	MemberStatusRemoved   MemberStatus = "removed"
)

type MemberSummary struct {
	UserID       string       `json:"user_id"`
	DisplayName  string       `json:"display_name"`
	Email        string       `json:"email,omitempty"`
	MembershipID string       `json:"membership_id"`
	IsOwner      bool         `json:"is_owner"`
	Status       MemberStatus `json:"status"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
	SuspendedAt  *time.Time   `json:"suspended_at,omitempty"`
	RemovedAt    *time.Time   `json:"removed_at,omitempty"`
}

type ListMembersResponse struct {
	Members []MemberSummary `json:"members"`
}

type UpdateMemberOwnershipRequest struct {
	IsOwner         bool `json:"is_owner"`
	ExpectedIsOwner bool `json:"expected_is_owner"`
}

type InvitationStatus string

const (
	InvitationStatusPending  InvitationStatus = "pending"
	InvitationStatusAccepted InvitationStatus = "accepted"
	InvitationStatusRevoked  InvitationStatus = "revoked"
	InvitationStatusExpired  InvitationStatus = "expired"
)

type InvitationSummary struct {
	ID                 string           `json:"id"`
	Email              string           `json:"email"`
	Status             InvitationStatus `json:"status"`
	InvitedByMemberID  string           `json:"invited_by_member_id,omitempty"`
	AcceptedByMemberID string           `json:"accepted_by_member_id,omitempty"`
	RevokedByMemberID  string           `json:"revoked_by_member_id,omitempty"`
	CreatedAt          time.Time        `json:"created_at"`
	ExpiresAt          time.Time        `json:"expires_at"`
	AcceptedAt         *time.Time       `json:"accepted_at,omitempty"`
	RevokedAt          *time.Time       `json:"revoked_at,omitempty"`
}

type ListInvitationsResponse struct {
	Invitations []InvitationSummary `json:"invitations"`
	NextCursor  string              `json:"next_cursor,omitempty"`
}

// CreateInvitationsRequest invites up to 50 email addresses with one role and
// expiry.
type CreateInvitationsRequest struct {
	Emails        []string `json:"emails"`
	ExpiresInDays *int     `json:"expires_in_days,omitempty"`
}

type InvitationResultStatus string

const (
	InvitationResultInvited InvitationResultStatus = "invited"
	InvitationResultFailed  InvitationResultStatus = "failed"
)

// InvitationResult is the outcome for one distinct address of the request:
// invited, meaning the invitation was emailed, or failed with its error, such
// as already_member, invitation_pending, invalid_email or email_failed. Each
// address succeeds or fails on its own.
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
