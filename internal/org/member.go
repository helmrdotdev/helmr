package org

import (
	"context"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"net/mail"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// MaxInvitationsPerRequest bounds the addresses one invitation request names.
// Each address is created under its own savepoint in the request's single
// transaction; staying below PostgreSQL's per-transaction cache of 64
// subtransactions keeps the request from overflowing it, which would slow
// every concurrent snapshot.
const MaxInvitationsPerRequest = 50

const (
	invitationTokenBytes        = 32
	defaultInvitationExpiryDays = 7
	maxInvitationExpiryDays     = 30
)

// ManagingMember identifies the acting human membership. Authority is read live.
type ManagingMember struct {
	Human       auth.Principal
	AuditPolicy auditpolicy.Policy
	Actor       audit.Evidence
	OrgID       uuid.UUID
	UserID      uuid.UUID
	MemberID    uuid.UUID
}

func (m *ManagingMember) Authorize(ctx context.Context, database db.DBTX, permission auth.Permission) error {
	p, err := m.Current(ctx, database)
	if err != nil {
		return err
	}
	checks, ok := p.Evaluate(permission, auth.Scope{OrgID: m.OrgID})
	m.Actor.Authorization = audit.PermissionAuthorization(checks, ok)
	if !ok {
		return errors.Join(ErrMemberManagementRequired, &auth.PermissionRefusal{Principal: p, Checks: checks})
	}

	return auth.RequireRecentFor(p, permission)
}

// Current retains the credential and membership that admitted this operation.
func (m *ManagingMember) Current(ctx context.Context, database db.DBTX) (auth.Principal, error) {
	if m.Human.OrgID != m.OrgID || m.Human.UserID != m.UserID || m.Human.MemberID != m.MemberID {
		return auth.Principal{}, ErrMemberManagementRequired
	}
	p, err := access.CurrentHuman(ctx, database, m.Human)
	if errors.Is(err, access.ErrDenied) {
		return p, ErrMemberManagementRequired
	}
	if err != nil {
		return p, err
	}
	m.Actor = audit.FromPrincipal(p)
	return p, nil
}

// InvitationInput describes one invitation request: the addresses to invite
// with one role and expiry. A nil ExpiresInDays takes the default expiry.
type InvitationInput struct {
	Emails        []string
	ExpiresInDays *int
}

// InvitationResult is one address of an invitation request: invited when Err
// is nil, otherwise refused or failed with Err.
type InvitationResult struct {
	Email string
	Err   error
}

// NewInvitation is an invitation created by the request with the raw token
// that only its invitee receives.
type NewInvitation struct {
	Invitation db.Invitation
	RawToken   string
}

// DeliverInvitations sends each new invitation of a request to its invitee. It
// runs inside the request's transaction before commit and returns one error
// per invitation, in order: nil when that invitation was sent.
type DeliverInvitations func(ctx context.Context, invitations []NewInvitation) []error

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
func ListMembers(ctx context.Context, database db.TxBeginner, manager ManagingMember) ([]db.ListOrgMembersRow, error) {
	var members []db.ListOrgMembersRow
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := manager.Authorize(ctx, tx, auth.PermissionMembersRead); err != nil {
			return err
		}
		var err error
		members, err = db.New(tx).ListOrgMembers(ctx, pgvalue.UUID(manager.OrgID))
		return err
	})
	return members, err
}

// ListInvitations reads the page under the same authority locks as admission.
func ListInvitations(ctx context.Context, database db.TxBeginner, manager ManagingMember, limit int32, after *InvitationPosition) ([]db.ListInvitationsRow, bool, error) {
	var invitations []db.ListInvitationsRow
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := manager.Authorize(ctx, tx, auth.PermissionInvitationsRead); err != nil {
			return err
		}
		params := db.ListInvitationsParams{OrgID: pgvalue.UUID(manager.OrgID), RowLimit: limit + 1}
		if after != nil {
			params.AfterCreatedAt = pgvalue.Timestamptz(after.CreatedAt)
			params.AfterID = pgvalue.UUID(after.ID)
		}
		var err error
		invitations, err = db.New(tx).ListInvitations(ctx, params)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if len(invitations) > int(limit) {
		return invitations[:limit], true, nil
	}
	return invitations, false, nil
}

// CreateInvitations invites each address of the request to the organization
// and returns one result per distinct address in request order. The request as
// a whole is rejected when the inviter cannot manage members, names no address
// or more than MaxInvitationsPerRequest, or gives an invalid role or expiry.
//
// An address is refused on its own when it is invalid, belongs to an active
// member or already has a pending invitation; expired invitations for an
// address are revoked first. Every other address is invited in one
// transaction, in address order: each invitation is written under a savepoint,
// so a refused address leaves the others in place, then deliver sends them, in
// the same order, before commit. An address whose email was not sent fails on
// its own with an error wrapping ErrInvitationNotDelivered and leaves no
// invitation; each sent invitation is recorded with its audit fact. Any other
// failure of the transaction fails every address it would have invited with
// that error. Raw tokens reach only deliver.
func CreateInvitations(ctx context.Context, database db.TxBeginner, tokenKey []byte, inviter ManagingMember, input InvitationInput, deliver DeliverInvitations) ([]InvitationResult, error) {
	if len(input.Emails) == 0 {
		return nil, invalidInput("emails must name at least one address")
	}
	if len(input.Emails) > MaxInvitationsPerRequest {
		return nil, invalidInput("emails must name at most %d addresses", MaxInvitationsPerRequest)
	}
	var err error
	expiresInDays := defaultInvitationExpiryDays
	if input.ExpiresInDays != nil {
		expiresInDays = *input.ExpiresInDays
	}
	if expiresInDays < 1 || expiresInDays > maxInvitationExpiryDays {
		return nil, invalidInput("expires_in_days must be between 1 and %d", maxInvitationExpiryDays)
	}
	results := make([]InvitationResult, 0, len(input.Emails))
	var valid []int
	seen := make(map[string]bool, len(input.Emails))
	for _, raw := range input.Emails {
		email, err := NormalizeEmail(raw)
		if err != nil {
			email = strings.TrimSpace(raw)
		}
		if seen[email] {
			continue
		}
		seen[email] = true
		if err == nil {
			valid = append(valid, len(results))
		}
		results = append(results, InvitationResult{Email: email, Err: err})
	}
	// Rows are written in address order so requests naming overlapping
	// addresses take their locks in the same order rather than deadlocking.
	ordered := slices.Clone(valid)
	slices.SortFunc(ordered, func(a, b int) int { return strings.Compare(results[a].Email, results[b].Email) })
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockAccessMembers(ctx, tx, &inviter, auth.PermissionInvitationsCreate); err != nil {
			return err
		}
		var batch []NewInvitation
		var addresses []int
		for _, i := range ordered {
			var invitation db.Invitation
			var rawToken string
			err := db.RunTx(ctx, tx, func(savepoint pgx.Tx) error {
				var err error
				invitation, rawToken, err = createInvitation(ctx, savepoint, tokenKey, inviter, results[i].Email, expiresInDays)
				return err
			})
			if invitationRefused(err) {
				results[i].Err = err
				continue
			}
			if err != nil {
				return err
			}
			batch = append(batch, NewInvitation{Invitation: invitation, RawToken: rawToken})
			addresses = append(addresses, i)
		}
		if len(batch) == 0 {
			return nil
		}
		sent := deliver(ctx, batch)
		if len(sent) != len(batch) {
			return fmt.Errorf("%w: delivery reported %d results for %d invitations", ErrInvitationNotDelivered, len(sent), len(batch))
		}
		for n, created := range batch {
			id := pgvalue.MustUUIDValue(created.Invitation.ID)
			if sent[n] != nil {
				// The invitation was never visible outside this transaction.
				if _, err := tx.Exec(ctx, `DELETE FROM invitations WHERE org_id=$1 AND id=$2`, inviter.OrgID, id); err != nil {
					return err
				}
				results[addresses[n]].Err = fmt.Errorf("%w: %w", ErrInvitationNotDelivered, sent[n])
				continue
			}
			if err := audit.Append(ctx, tx, inviter.AuditPolicy, audit.Event{Evidence: inviter.Actor, OrgID: inviter.OrgID, Action: "invitation.created", TargetKind: "invitation", TargetID: id, Details: audit.InvitationCreatedDetails{ExpiresAt: pgvalue.TimePtr(created.Invitation.ExpiresAt)}}); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, ErrMemberManagementRequired) {
		return nil, err
	}
	if err != nil {
		for _, i := range valid {
			if results[i].Err == nil {
				results[i].Err = err
			}
		}
	}
	return results, nil
}

// invitationRefused reports whether an address was refused on its own rather
// than by a failure of the request's transaction.
func invitationRefused(err error) bool {
	return errors.Is(err, ErrInvitationActiveMember) || errors.Is(err, ErrInvitationPending) || errors.Is(err, ErrOwnerRoleRequired)
}

func createInvitation(ctx context.Context, tx pgx.Tx, tokenKey []byte, inviter ManagingMember, email string, expiresInDays int) (db.Invitation, string, error) {
	q := db.New(tx)
	expired, err := q.RevokeExpiredInvitationsByEmail(ctx, db.RevokeExpiredInvitationsByEmailParams{
		OrgID:        pgvalue.UUID(inviter.OrgID),
		InviteeEmail: email,
	})
	if err != nil {
		return db.Invitation{}, "", fmt.Errorf("expire invitations: %w", err)
	}
	for _, old := range expired {
		if err := audit.Append(ctx, tx, inviter.AuditPolicy, audit.Event{Evidence: audit.Internal(audit.ControlPlane), OrgID: inviter.OrgID, Action: "invitation.expired", TargetKind: "invitation", TargetID: pgvalue.MustUUIDValue(old), Details: audit.InvitationStateDetails{}}); err != nil {
			return db.Invitation{}, "", err
		}
	}
	_, found, err := pendingInvitation(ctx, q, inviter.OrgID, email)
	if err != nil {
		return db.Invitation{}, "", err
	}
	if found {
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
		ID:                pgvalue.UUID(uuid.NewV7()),
		OrgID:             pgvalue.UUID(inviter.OrgID),
		InviteeEmail:      email,
		InvitedByMemberID: pgvalue.UUID(inviter.MemberID),
		TokenHash:         tokenHash,
		ExpiresAt:         pgvalue.Timestamptz(time.Now().AddDate(0, 0, expiresInDays)),
	})
	if isNoRows(err) {
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
func RevokeInvitation(ctx context.Context, database db.TxBeginner, manager ManagingMember, invitationID uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockAccessMembers(ctx, tx, &manager, auth.PermissionInvitationsRevoke); err != nil {
			return err
		}
		return revokeInvitation(ctx, tx, manager, invitationID)
	})
}
func revokeInvitation(ctx context.Context, tx pgx.Tx, manager ManagingMember, invitationID uuid.UUID) error {
	q := db.New(tx)
	_, err := q.GetRevocableInvitation(ctx, db.GetRevocableInvitationParams{
		OrgID: pgvalue.UUID(manager.OrgID),
		ID:    pgvalue.UUID(invitationID),
	})
	if isNoRows(err) {
		return ErrInvitationNotFound
	}
	if err != nil {
		return fmt.Errorf("load invitation: %w", err)
	}
	revoked, err := q.RevokeInvitation(ctx, db.RevokeInvitationParams{
		OrgID:             pgvalue.UUID(manager.OrgID),
		ID:                pgvalue.UUID(invitationID),
		RevokedByMemberID: pgvalue.UUID(manager.MemberID),
	})
	if err != nil {
		return fmt.Errorf("revoke invitation: %w", err)
	}
	if revoked == 0 {
		return ErrInvitationNotFound
	}
	return audit.Append(ctx, tx, manager.AuditPolicy, audit.Event{Evidence: manager.Actor, OrgID: manager.OrgID, Action: "invitation.revoked", TargetKind: "invitation", TargetID: invitationID, Details: audit.InvitationStateDetails{}})
}

// UpdateMemberOwnership serializes owner changes on the organization row.
// Ownership conveys recovery authority, not ordinary management permission.
func UpdateMemberOwnership(ctx context.Context, database db.TxBeginner, manager ManagingMember, targetID uuid.UUID, value, expected bool) (UpdatedMember, error) {
	var result UpdatedMember
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, manager.OrgID); err != nil {
			return err
		}
		if err := lockActiveMembers(ctx, tx, manager, targetID); err != nil {
			return err
		}
		current, err := manager.Current(ctx, tx)
		if err != nil {
			return err
		}
		if err := auth.RequireRecentProof(current); err != nil {
			return err
		}
		q := db.New(tx)
		actor, err := q.GetActiveOrgMemberByUser(ctx, db.GetActiveOrgMemberByUserParams{OrgID: pgvalue.UUID(manager.OrgID), UserID: pgvalue.UUID(manager.UserID)})
		if isNoRows(err) {
			return ErrOwnerRoleRequired
		}
		if err != nil {
			return err
		}
		if !actor.IsOwner {
			evidence := manager.Actor
			evidence.Authorization = audit.Ownership(auth.Principal{OrgID: manager.OrgID, MemberID: pgvalue.MustUUIDValue(actor.ID), IsOwner: false})
			return &audit.Refusal{Authorization: evidence.Authorization, Cause: ErrOwnerRoleRequired}
		}
		if pgvalue.MustUUIDValue(actor.ID) != manager.MemberID {
			return ErrOwnerRoleRequired
		}
		manager.Actor.Authorization = audit.Ownership(auth.Principal{OrgID: manager.OrgID, MemberID: manager.MemberID, IsOwner: actor.IsOwner})
		target, err := activeMember(ctx, q, manager.OrgID, targetID)
		if err != nil {
			return err
		}
		if value && target.SuspendedAt.Valid {
			return invalidInput("resume the member before granting ownership")
		}
		if target.IsOwner != expected {
			return ErrMemberRoleChanged
		}
		if target.IsOwner && !value {
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM org_members WHERE org_id=$1 AND is_owner AND removed_at IS NULL AND suspended_at IS NULL`, manager.OrgID).Scan(&count); err != nil {
				return err
			}
			if count <= 1 {
				return ErrLastActiveOwner
			}
		}
		updated, err := q.UpdateOrgMemberOwnership(ctx, db.UpdateOrgMemberOwnershipParams{OrgID: pgvalue.UUID(manager.OrgID), ID: pgvalue.UUID(targetID), IsOwner: value, ExpectedIsOwner: expected})
		if isNoRows(err) {
			return ErrMemberRoleChanged
		}
		if err != nil {
			return err
		}
		result = UpdatedMember{Member: updated, DisplayName: target.DisplayName, PrimaryEmail: target.PrimaryEmail}
		if expected == value {
			return nil
		}
		return audit.Append(ctx, tx, manager.AuditPolicy, audit.Event{Evidence: manager.Actor, OrgID: manager.OrgID, Action: "member.ownership_changed", TargetKind: "member", TargetID: targetID, Details: audit.OwnershipChangedDetails{WasOwner: expected, IsOwner: value}})
	})
	return result, err
}

// DisableMember removes one membership and its explicit access atomically.
// An owner must relinquish ownership before this operation.
func DisableMember(ctx context.Context, tx pgx.Tx, manager ManagingMember, targetID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, manager.OrgID); err != nil {
		return err
	}
	if err := lockAccessMembers(ctx, tx, &manager, auth.PermissionMembersRemove, targetID); err != nil {
		return err
	}
	q := db.New(tx)
	target, err := activeMember(ctx, q, manager.OrgID, targetID)
	if err != nil {
		return err
	}
	if target.IsOwner {
		return ErrOwnerRoleRequired
	}
	grants, err := readRoleGrants(ctx, tx, manager.OrgID, GrantFilter{MemberID: targetID})
	if err != nil {
		return err
	}
	for _, grant := range grants {
		if err := accessAudit(ctx, tx, manager, "role_grant.deleted", "role_grant", grant.ID, grantSnapshot(grant), nil); err != nil {
			return err
		}
	}
	if _, err = q.RemoveOrgMember(ctx, db.RemoveOrgMemberParams{OrgID: pgvalue.UUID(manager.OrgID), ID: pgvalue.UUID(targetID)}); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM role_grants WHERE org_id=$1 AND member_id=$2`, manager.OrgID, targetID); err != nil {
		return err
	}
	if err = RemovePermissionGroupMemberships(ctx, tx, manager, targetID); err != nil {
		return err
	}
	return audit.Append(ctx, tx, manager.AuditPolicy, audit.Event{Evidence: manager.Actor, OrgID: manager.OrgID, Action: "member.removed", TargetKind: "member", TargetID: targetID, Details: audit.MembershipDetails{MembershipID: targetID.String(), UserID: pgvalue.UUIDString(target.UserID), DisplayName: target.DisplayName.String}})
}

func activeMember(ctx context.Context, q db.Querier, orgID, memberID uuid.UUID) (db.GetOrgMemberForManagementRow, error) {
	member, err := q.GetOrgMemberForManagement(ctx, db.GetOrgMemberForManagementParams{OrgID: pgvalue.UUID(orgID), ID: pgvalue.UUID(memberID)})
	if isNoRows(err) {
		return member, ErrMemberNotFound
	}
	if err != nil {
		return member, err
	}
	if member.RemovedAt.Valid {
		return member, ErrMemberNotFound
	}
	return member, nil
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
	return pendingInvitationFromRow(row.ID, row.OrgID, row.InviteeEmail)
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
	return pendingInvitationFromRow(row.ID, row.OrgID, row.InviteeEmail)
}

func pendingInvitationFromRow(id pgtype.UUID, orgID pgtype.UUID, inviteeEmail string) (PendingInvitation, error) {
	invitationID, err := pgvalue.UUIDValue(id)
	if err != nil {
		return PendingInvitation{}, fmt.Errorf("invitation id: %w", err)
	}
	invitationOrgID, err := pgvalue.UUIDValue(orgID)
	if err != nil {
		return PendingInvitation{}, fmt.Errorf("invitation org id: %w", err)
	}
	return PendingInvitation{ID: invitationID, OrgID: invitationOrgID, InviteeEmail: inviteeEmail}, nil
}

// AcceptInvitation creates a new membership; removed membership IDs never revive.
func AcceptInvitation(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, actor audit.Evidence, invitation PendingInvitation, userID uuid.UUID, displayName string) error {
	if _, err := tx.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, invitation.OrgID); err != nil {
		return err
	}
	q := db.New(tx)
	_, err := q.GetActiveOrgMemberByUser(ctx, db.GetActiveOrgMemberByUserParams{OrgID: pgvalue.UUID(invitation.OrgID), UserID: pgvalue.UUID(userID)})
	if err == nil {
		return ErrAlreadyMember
	}
	if !isNoRows(err) {
		return err
	}
	member, err := q.EnsureOrgMember(ctx, db.EnsureOrgMemberParams{ID: pgvalue.NewUUIDv7(), OrgID: pgvalue.UUID(invitation.OrgID), UserID: pgvalue.UUID(userID), IsOwner: false, DisplayName: pgtype.Text{String: displayName, Valid: displayName != ""}})
	if err != nil {
		return err
	}
	accepted, err := q.AcceptInvitation(ctx, db.AcceptInvitationParams{OrgID: pgvalue.UUID(invitation.OrgID), ID: pgvalue.UUID(invitation.ID), MemberID: member.ID})
	if err != nil {
		return err
	}
	if accepted == 0 {
		return ErrInvitationNotFound
	}
	return audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, OrgID: invitation.OrgID, Action: "invitation.accepted", TargetKind: "invitation", TargetID: invitation.ID, TargetUserID: userID, Details: audit.MembershipDetails{MembershipID: pgvalue.UUIDString(member.ID)}})
}

// AcceptExistingMemberInvitation records consent without changing membership grants.
// The caller holds the organization, invitation and account locks.
func AcceptExistingMemberInvitation(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, actor audit.Evidence, id, orgID, memberID, userID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE invitations SET accepted_at=clock_timestamp(),accepted_by_member_id=$2 WHERE id=$1`, id, memberID); err != nil {
		return err
	}
	return audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, OrgID: orgID, Action: "invitation.accepted", TargetKind: "invitation", TargetID: id, TargetUserID: userID, Details: audit.MembershipDetails{MembershipID: memberID.String()}})
}
