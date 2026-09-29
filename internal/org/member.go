package org

import (
	"context"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	invitationTokenBytes        = 32
	defaultInvitationExpiryDays = 7
	maxInvitationExpiryDays     = 30
)

// ManagingMember is the organization member performing member management.
// Its role must allow member management; owners alone manage owner
// memberships and owner invitations.
type ManagingMember struct {
	OrgID  uuid.UUID
	UserID uuid.UUID
	Role   auth.Role
}

func (m ManagingMember) authorize() error {
	if !auth.RoleAllows(m.Role, auth.PermissionMembersManage) {
		return ErrMemberManagementRequired
	}
	return nil
}

func (m ManagingMember) isOwner() bool {
	return m.Role == auth.RoleOwner
}

// InvitationInput describes an invitation to create. A nil ExpiresInDays takes
// the default expiry.
type InvitationInput struct {
	Email         string
	Role          string
	ExpiresInDays *int
}

// InvitationPosition is the sort key of the last invitation on a previous page.
type InvitationPosition struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// UpdatedMember is a member after a role change with the user profile fields
// read for it.
type UpdatedMember struct {
	Member       db.OrgMember
	DisplayName  pgtype.Text
	PrimaryEmail pgtype.Text
}

// ListMembers returns every member of the organization, including disabled ones.
func ListMembers(ctx context.Context, q db.Querier, manager ManagingMember) ([]db.ListOrgMembersRow, error) {
	if err := manager.authorize(); err != nil {
		return nil, err
	}
	members, err := q.ListOrgMembers(ctx, pgvalue.UUID(manager.OrgID))
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	return members, nil
}

// ListInvitations returns up to limit invitations of the organization after
// the given position and whether more follow.
func ListInvitations(ctx context.Context, q db.Querier, manager ManagingMember, limit int32, after *InvitationPosition) ([]db.ListInvitationsRow, bool, error) {
	if err := manager.authorize(); err != nil {
		return nil, false, err
	}
	params := db.ListInvitationsParams{OrgID: pgvalue.UUID(manager.OrgID), RowLimit: limit + 1}
	if after != nil {
		params.AfterCreatedAt = pgvalue.Timestamptz(after.CreatedAt)
		params.AfterID = pgvalue.UUID(after.ID)
	}
	invitations, err := q.ListInvitations(ctx, params)
	if err != nil {
		return nil, false, fmt.Errorf("list invitations: %w", err)
	}
	if len(invitations) > int(limit) {
		return invitations[:limit], true, nil
	}
	return invitations, false, nil
}

// CreateInvitation invites an email address to the organization and returns
// the invitation with its raw token, which only the invitee link carries.
// Expired invitations for the address are revoked first; a pending invitation
// or an active member with the address rejects the new one.
func CreateInvitation(ctx context.Context, q db.Querier, tokenKey []byte, inviter ManagingMember, input InvitationInput) (db.Invitation, string, error) {
	if err := inviter.authorize(); err != nil {
		return db.Invitation{}, "", err
	}
	email, err := NormalizeEmail(input.Email)
	if err != nil {
		return db.Invitation{}, "", err
	}
	role, err := parseMemberRole(input.Role, "role")
	if err != nil {
		return db.Invitation{}, "", err
	}
	expiresInDays := defaultInvitationExpiryDays
	if input.ExpiresInDays != nil {
		expiresInDays = *input.ExpiresInDays
	}
	if expiresInDays < 1 || expiresInDays > maxInvitationExpiryDays {
		return db.Invitation{}, "", invalidInput("expires_in_days must be between 1 and %d", maxInvitationExpiryDays)
	}
	if role == db.OrgMemberRoleOwner && !inviter.isOwner() {
		return db.Invitation{}, "", ErrOwnerRoleRequired
	}
	if _, err := q.RevokeExpiredInvitationsByEmail(ctx, db.RevokeExpiredInvitationsByEmailParams{
		OrgID:        pgvalue.UUID(inviter.OrgID),
		InviteeEmail: email,
	}); err != nil {
		return db.Invitation{}, "", fmt.Errorf("expire invitations: %w", err)
	}
	pending, found, err := pendingInvitation(ctx, q, inviter.OrgID, email)
	if err != nil {
		return db.Invitation{}, "", err
	}
	if found {
		if pending.Role == db.OrgMemberRoleOwner && !inviter.isOwner() {
			return db.Invitation{}, "", ErrOwnerRoleRequired
		}
		return db.Invitation{}, "", ErrInvitationPending
	}
	rawToken, err := auth.GenerateOpaque(invitationTokenBytes)
	if err != nil {
		return db.Invitation{}, "", fmt.Errorf("generate invitation token: %w", err)
	}
	tokenHash, err := auth.HashToken(tokenKey, rawToken)
	if err != nil {
		return db.Invitation{}, "", fmt.Errorf("hash invitation token: %w", err)
	}
	invitation, err := q.CreateInvitation(ctx, db.CreateInvitationParams{
		ID:              pgvalue.UUID(uuid.NewV7()),
		OrgID:           pgvalue.UUID(inviter.OrgID),
		InviteeEmail:    email,
		Role:            role,
		InvitedByUserID: pgvalue.UUID(inviter.UserID),
		TokenHash:       tokenHash,
		ExpiresAt:       pgvalue.Timestamptz(time.Now().AddDate(0, 0, expiresInDays)),
	})
	if isNoRows(err) {
		// The insert admits no row when the address already belongs to an
		// active member. A pending owner invitation for the address answers a
		// non-owner inviter the same way as before the insert.
		pending, found, err := pendingInvitation(ctx, q, inviter.OrgID, email)
		if err == nil && found && pending.Role == db.OrgMemberRoleOwner && !inviter.isOwner() {
			return db.Invitation{}, "", ErrOwnerRoleRequired
		}
		return db.Invitation{}, "", ErrInvitationActiveMember
	}
	if db.IsUniqueViolation(err) {
		return db.Invitation{}, "", ErrInvitationPending
	}
	if err != nil {
		return db.Invitation{}, "", fmt.Errorf("create invitation: %w", err)
	}
	return invitation, rawToken, nil
}

func pendingInvitation(ctx context.Context, q db.Querier, orgID uuid.UUID, email string) (db.GetPendingInvitationByEmailRow, bool, error) {
	pending, err := q.GetPendingInvitationByEmail(ctx, db.GetPendingInvitationByEmailParams{
		OrgID:        pgvalue.UUID(orgID),
		InviteeEmail: email,
	})
	if isNoRows(err) {
		return db.GetPendingInvitationByEmailRow{}, false, nil
	}
	if err != nil {
		return db.GetPendingInvitationByEmailRow{}, false, fmt.Errorf("load invitation: %w", err)
	}
	return pending, true, nil
}

// RevokeInvitation revokes a pending invitation of the organization.
func RevokeInvitation(ctx context.Context, q db.Querier, manager ManagingMember, invitationID uuid.UUID) error {
	if err := manager.authorize(); err != nil {
		return err
	}
	invitation, err := q.GetRevocableInvitation(ctx, db.GetRevocableInvitationParams{
		OrgID: pgvalue.UUID(manager.OrgID),
		ID:    pgvalue.UUID(invitationID),
	})
	if isNoRows(err) {
		return ErrInvitationNotFound
	}
	if err != nil {
		return fmt.Errorf("load invitation: %w", err)
	}
	if invitation.Role == db.OrgMemberRoleOwner && !manager.isOwner() {
		return ErrOwnerRoleRequired
	}
	revoked, err := q.RevokeInvitation(ctx, db.RevokeInvitationParams{
		OrgID:           pgvalue.UUID(manager.OrgID),
		ID:              pgvalue.UUID(invitationID),
		RevokedByUserID: pgvalue.UUID(manager.UserID),
	})
	if err != nil {
		return fmt.Errorf("revoke invitation: %w", err)
	}
	if revoked == 0 {
		return ErrInvitationNotFound
	}
	return nil
}

// UpdateMemberRole changes an active member's role when it still holds
// expectedRole. Managers cannot drop their own member management access, only
// owners change owner roles, and the last active owner cannot be demoted.
func UpdateMemberRole(ctx context.Context, q db.Querier, manager ManagingMember, targetUserID uuid.UUID, role string, expectedRole string) (UpdatedMember, error) {
	if err := manager.authorize(); err != nil {
		return UpdatedMember{}, err
	}
	newRole, err := parseMemberRole(role, "role")
	if err != nil {
		return UpdatedMember{}, err
	}
	currentRole, err := parseMemberRole(expectedRole, "expected_role")
	if err != nil {
		return UpdatedMember{}, err
	}
	target, err := activeMember(ctx, q, manager.OrgID, targetUserID)
	if err != nil {
		return UpdatedMember{}, err
	}
	if target.Role != currentRole {
		return UpdatedMember{}, ErrMemberRoleChanged
	}
	if manager.UserID == targetUserID && !auth.RoleAllows(auth.Role(newRole), auth.PermissionMembersManage) {
		return UpdatedMember{}, ErrSelfMemberManagement
	}
	if (currentRole == db.OrgMemberRoleOwner || newRole == db.OrgMemberRoleOwner) && !manager.isOwner() {
		return UpdatedMember{}, ErrOwnerRoleRequired
	}
	updated, err := q.UpdateOrgMemberRole(ctx, db.UpdateOrgMemberRoleParams{
		OrgID:        pgvalue.UUID(manager.OrgID),
		UserID:       pgvalue.UUID(targetUserID),
		Role:         newRole,
		ExpectedRole: currentRole,
		ActorIsOwner: manager.isOwner(),
	})
	if isNoRows(err) {
		if currentRole == db.OrgMemberRoleOwner && newRole != db.OrgMemberRoleOwner {
			return UpdatedMember{}, ErrLastActiveOwner
		}
		return UpdatedMember{}, ErrMemberRoleChanged
	}
	if err != nil {
		return UpdatedMember{}, fmt.Errorf("update member role: %w", err)
	}
	return UpdatedMember{Member: updated, DisplayName: target.DisplayName, PrimaryEmail: target.PrimaryEmail}, nil
}

// RemoveMember disables an active member and revokes the member's sessions for
// the organization in one statement. Managers cannot remove themselves, only
// owners remove owners, and the last active owner cannot be removed.
func RemoveMember(ctx context.Context, q db.Querier, manager ManagingMember, targetUserID uuid.UUID) error {
	if err := manager.authorize(); err != nil {
		return err
	}
	if manager.UserID == targetUserID {
		return ErrSelfMemberRemoval
	}
	target, err := activeMember(ctx, q, manager.OrgID, targetUserID)
	if err != nil {
		return err
	}
	if target.Role == db.OrgMemberRoleOwner && !manager.isOwner() {
		return ErrOwnerRoleRequired
	}
	_, err = q.DisableOrgMemberAndRevokeOrgSessions(ctx, db.DisableOrgMemberAndRevokeOrgSessionsParams{
		OrgID:        pgvalue.UUID(manager.OrgID),
		UserID:       pgvalue.UUID(targetUserID),
		ExpectedRole: target.Role,
		ActorIsOwner: manager.isOwner(),
	})
	if isNoRows(err) {
		if target.Role == db.OrgMemberRoleOwner {
			return ErrLastActiveOwner
		}
		return ErrMemberNotFound
	}
	if err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	return nil
}

// activeMember loads a member whose membership and user are both enabled.
func activeMember(ctx context.Context, q db.Querier, orgID uuid.UUID, userID uuid.UUID) (db.GetOrgMemberForManagementRow, error) {
	member, err := q.GetOrgMemberForManagement(ctx, db.GetOrgMemberForManagementParams{
		OrgID:  pgvalue.UUID(orgID),
		UserID: pgvalue.UUID(userID),
	})
	if isNoRows(err) {
		return db.GetOrgMemberForManagementRow{}, ErrMemberNotFound
	}
	if err != nil {
		return db.GetOrgMemberForManagementRow{}, fmt.Errorf("load member: %w", err)
	}
	if member.DisabledAt.Valid || member.UserDisabledAt.Valid {
		return db.GetOrgMemberForManagementRow{}, ErrMemberNotFound
	}
	return member, nil
}

func parseMemberRole(value string, field string) (db.OrgMemberRole, error) {
	switch role := db.OrgMemberRole(strings.TrimSpace(value)); role {
	case db.OrgMemberRoleOwner, db.OrgMemberRoleAdmin, db.OrgMemberRoleDeveloper, db.OrgMemberRoleViewer:
		return role, nil
	default:
		return "", invalidInput("%s must be owner, admin, developer, or viewer", field)
	}
}

// NormalizeEmail validates a bare email address and returns it lowercased.
func NormalizeEmail(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 320 {
		return "", invalidInput("email is required")
	}
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address != value {
		return "", invalidInput("email must be a valid address")
	}
	return strings.ToLower(address.Address), nil
}

// PendingInvitation is an invitation that can still be accepted.
type PendingInvitation struct {
	ID           uuid.UUID
	OrgID        uuid.UUID
	InviteeEmail string
	Role         db.OrgMemberRole
}

// PendingInvitationByTokenHash loads the pending invitation whose token hashes
// to tokenHash. A missing, accepted, revoked or expired invitation is
// ErrInvitationNotFound.
func PendingInvitationByTokenHash(ctx context.Context, q db.Querier, tokenHash []byte) (PendingInvitation, error) {
	row, err := q.GetActiveInvitation(ctx, tokenHash)
	if isNoRows(err) {
		return PendingInvitation{}, ErrInvitationNotFound
	}
	if err != nil {
		return PendingInvitation{}, fmt.Errorf("load invitation: %w", err)
	}
	return pendingInvitationFromRow(row.ID, row.OrgID, row.InviteeEmail, row.Role)
}

// PendingInvitationByID loads a pending invitation by its ID. A missing,
// accepted, revoked or expired invitation is ErrInvitationNotFound.
func PendingInvitationByID(ctx context.Context, q db.Querier, id uuid.UUID) (PendingInvitation, error) {
	row, err := q.GetActiveInvitationByID(ctx, pgvalue.UUID(id))
	if isNoRows(err) {
		return PendingInvitation{}, ErrInvitationNotFound
	}
	if err != nil {
		return PendingInvitation{}, fmt.Errorf("load invitation: %w", err)
	}
	return pendingInvitationFromRow(row.ID, row.OrgID, row.InviteeEmail, row.Role)
}

func pendingInvitationFromRow(id pgtype.UUID, orgID pgtype.UUID, inviteeEmail string, role db.OrgMemberRole) (PendingInvitation, error) {
	invitationID, err := pgvalue.UUIDValue(id)
	if err != nil {
		return PendingInvitation{}, fmt.Errorf("invitation id: %w", err)
	}
	invitationOrgID, err := pgvalue.UUIDValue(orgID)
	if err != nil {
		return PendingInvitation{}, fmt.Errorf("invitation org id: %w", err)
	}
	return PendingInvitation{ID: invitationID, OrgID: invitationOrgID, InviteeEmail: inviteeEmail, Role: role}, nil
}

// AcceptInvitation makes the user a member of the invitation's organization
// with the invited role. It runs on the caller's transaction and requires one:
// the accepted invitation references the membership written after it, which
// the database checks at commit. It rejects a user who is already an active
// member, marks the invitation accepted, writes the membership, re-enabling a
// disabled one, and then revokes the user's existing login sessions. An
// invitation accepted, revoked or expired since it was loaded is
// ErrInvitationNotFound.
//
// Paths that write both a membership and login sessions lock org_members rows
// before auth_sessions rows, as member removal does, so that acceptance and
// removal of the same user cannot deadlock.
func AcceptInvitation(ctx context.Context, q db.Querier, invitation PendingInvitation, userID uuid.UUID, displayName string) error {
	existing, err := q.GetOrgMemberForManagement(ctx, db.GetOrgMemberForManagementParams{
		OrgID:  pgvalue.UUID(invitation.OrgID),
		UserID: pgvalue.UUID(userID),
	})
	if err != nil && !isNoRows(err) {
		return fmt.Errorf("load member: %w", err)
	}
	if err == nil && !existing.DisabledAt.Valid {
		if existing.UserDisabledAt.Valid {
			return ErrUserDisabled
		}
		return ErrAlreadyMember
	}
	accepted, err := q.AcceptInvitation(ctx, db.AcceptInvitationParams{
		OrgID:  pgvalue.UUID(invitation.OrgID),
		ID:     pgvalue.UUID(invitation.ID),
		UserID: pgvalue.UUID(userID),
	})
	if err != nil {
		return fmt.Errorf("accept invitation: %w", err)
	}
	if accepted == 0 {
		return ErrInvitationNotFound
	}
	if _, err := q.EnsureOrgMember(ctx, db.EnsureOrgMemberParams{
		OrgID:       pgvalue.UUID(invitation.OrgID),
		UserID:      pgvalue.UUID(userID),
		Role:        invitation.Role,
		DisplayName: pgtype.Text{String: displayName, Valid: displayName != ""},
	}); err != nil {
		return fmt.Errorf("add member: %w", err)
	}
	if _, err := q.RevokeAuthSessionsForUser(ctx, pgvalue.UUID(userID)); err != nil {
		return fmt.Errorf("revoke login sessions: %w", err)
	}
	return nil
}
