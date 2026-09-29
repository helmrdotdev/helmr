package identity

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	magicLinkTokenBytes      = 32
	magicLinkRateLimitWindow = 15 * time.Minute
	magicLinkRateLimitCount  = int64(5)
	magicLinkProvider        = "magic-link"
)

// MagicLinkRecipient is who a magic link signs in and why. An invitation link
// names the invitation and its organization; a login link names neither.
type MagicLinkRecipient struct {
	Purpose      db.MagicLinkPurpose
	Email        string
	OrgID        uuid.UUID
	InvitationID uuid.UUID
}

func (r MagicLinkRecipient) orgID() pgtype.UUID {
	if r.OrgID == uuid.Nil() {
		return pgtype.UUID{}
	}
	return pgvalue.UUID(r.OrgID)
}

func (r MagicLinkRecipient) invitationID() pgtype.UUID {
	if r.InvitationID == uuid.Nil() {
		return pgtype.UUID{}
	}
	return pgvalue.UUID(r.InvitationID)
}

// PendingMagicLink is a created magic link awaiting delivery. Token is the raw
// token the delivered link carries.
type PendingMagicLink struct {
	ID        uuid.UUID
	Recipient MagicLinkRecipient
	Token     string
	ExpiresAt time.Time
}

// CreateMagicLink creates a pending magic link for the recipient, returning
// false without creating one when the recipient's address reached the rate
// limit for the purpose. Link creation for one recipient is serialized by a
// transaction-scoped advisory lock taken as the transaction's first statement.
// redirectAfter is the destination the completed sign-in returns to; empty
// means none.
func CreateMagicLink(ctx context.Context, txb db.TxBeginner, cfg Config, recipient MagicLinkRecipient, redirectAfter string) (PendingMagicLink, bool, error) {
	var link PendingMagicLink
	created := false
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := lockMagicLinkRecipient(ctx, q, recipient); err != nil {
			return err
		}
		count, err := q.CountRecentMagicLinks(ctx, db.CountRecentMagicLinksParams{
			Purpose: recipient.Purpose,
			Email:   recipient.Email,
			Since:   pgvalue.Timestamptz(time.Now().Add(-magicLinkRateLimitWindow)),
		})
		if err != nil {
			return fmt.Errorf("count recent magic links: %w", err)
		}
		if count >= magicLinkRateLimitCount {
			return nil
		}
		rawToken, err := auth.GenerateOpaque(magicLinkTokenBytes)
		if err != nil {
			return fmt.Errorf("generate magic link token: %w", err)
		}
		tokenHash, err := auth.HashToken(cfg.magicLinkKey, rawToken)
		if err != nil {
			return fmt.Errorf("hash magic link token: %w", err)
		}
		redirect := pgtype.Text{}
		if redirectAfter != "" {
			redirect = pgtype.Text{String: redirectAfter, Valid: true}
		}
		expiresAt := time.Now().Add(cfg.lifetimes.MagicLink)
		row, err := q.CreateMagicLink(ctx, db.CreateMagicLinkParams{
			ID:            pgvalue.UUID(uuid.NewV7()),
			Purpose:       recipient.Purpose,
			TokenHash:     tokenHash,
			Email:         recipient.Email,
			OrgID:         recipient.orgID(),
			InvitationID:  recipient.invitationID(),
			RedirectAfter: redirect,
			ExpiresAt:     pgvalue.Timestamptz(expiresAt),
		})
		if err != nil {
			return fmt.Errorf("create magic link: %w", err)
		}
		linkID, err := pgvalue.UUIDValue(row.ID)
		if err != nil {
			return fmt.Errorf("magic link id: %w", err)
		}
		link = PendingMagicLink{ID: linkID, Recipient: recipient, Token: rawToken, ExpiresAt: expiresAt}
		created = true
		return nil
	})
	if err != nil {
		return PendingMagicLink{}, false, err
	}
	return link, created, nil
}

// MarkMagicLinkSent records the delivery of a pending magic link and revokes
// the recipient's older open links. A link that is no longer pending is an
// error.
func MarkMagicLinkSent(ctx context.Context, txb db.TxBeginner, link PendingMagicLink) error {
	return db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := lockMagicLinkRecipient(ctx, q, link.Recipient); err != nil {
			return err
		}
		marked, err := q.MarkMagicLinkSent(ctx, pgvalue.UUID(link.ID))
		if err != nil {
			return fmt.Errorf("mark magic link sent: %w", err)
		}
		if marked != 1 {
			return errors.New("mark magic link sent")
		}
		if _, err := q.RevokeOpenMagicLinksForRecipient(ctx, db.RevokeOpenMagicLinksForRecipientParams{
			Purpose:      link.Recipient.Purpose,
			Email:        link.Recipient.Email,
			OrgID:        link.Recipient.orgID(),
			InvitationID: link.Recipient.invitationID(),
			ExceptID:     pgvalue.UUID(link.ID),
		}); err != nil {
			return fmt.Errorf("revoke older magic links: %w", err)
		}
		return nil
	})
}

// MarkMagicLinkDeliveryFailed revokes a pending magic link whose delivery
// failed. It reports whether the link was still pending.
func MarkMagicLinkDeliveryFailed(ctx context.Context, q db.Querier, linkID uuid.UUID) (bool, error) {
	marked, err := q.MarkMagicLinkDeliveryFailed(ctx, pgvalue.UUID(linkID))
	if err != nil {
		return false, fmt.Errorf("mark magic link delivery failed: %w", err)
	}
	return marked == 1, nil
}

func lockMagicLinkRecipient(ctx context.Context, q db.Querier, recipient MagicLinkRecipient) error {
	if err := q.LockMagicLinkRecipient(ctx, magicLinkRecipientLockKey(recipient)); err != nil {
		return fmt.Errorf("lock magic link recipient: %w", err)
	}
	return nil
}

func magicLinkRecipientLockKey(recipient MagicLinkRecipient) int64 {
	orgID := recipient.orgID()
	invitationID := recipient.invitationID()
	h := fnv.New64a()
	_, _ = h.Write([]byte("helmr.magic_link.start\x00"))
	_, _ = h.Write([]byte(recipient.Purpose))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(recipient.Email))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(orgID.Bytes[:])
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(invitationID.Bytes[:])
	return int64(h.Sum64() & math.MaxInt64)
}

// CompletedMagicLink is the outcome of a completed magic link: the raw token of
// the issued login session and the destination the link was created with,
// empty when none.
type CompletedMagicLink struct {
	Session       string
	RedirectAfter string
}

// CompleteMagicLink consumes a delivered magic link once and signs its
// recipient in. A login link issues a session that selects no organization; an
// invitation link accepts the invitation and issues a session for its
// organization. An unknown, consumed, revoked or expired link is
// ErrInvalidToken.
func CompleteMagicLink(ctx context.Context, txb db.TxBeginner, cfg Config, rawToken string) (CompletedMagicLink, error) {
	tokenHash, err := auth.HashToken(cfg.magicLinkKey, rawToken)
	if err != nil {
		return CompletedMagicLink{}, ErrInvalidToken
	}
	var completed CompletedMagicLink
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		link, err := q.GetActiveMagicLinkByTokenHash(ctx, tokenHash)
		if isNoRows(err) {
			return ErrInvalidToken
		}
		if err != nil {
			return fmt.Errorf("load magic link: %w", err)
		}
		external := magicLinkIdentity(link.Email)
		var invitation *org.PendingInvitation
		switch link.Purpose {
		case db.MagicLinkPurposeInviteAccept:
			if !link.InvitationID.Valid {
				return ErrInvalidToken
			}
			invitationID, err := pgvalue.UUIDValue(link.InvitationID)
			if err != nil {
				return fmt.Errorf("magic link invitation id: %w", err)
			}
			pending, err := org.PendingInvitationByID(ctx, q, invitationID)
			if err != nil {
				return acceptanceError(err)
			}
			if !external.Verifies(pending.InviteeEmail) {
				return ErrWrongAccount
			}
			invitation = &pending
		case db.MagicLinkPurposeLogin:
		default:
			return errors.New("unknown magic link purpose")
		}
		user, err := upsertMagicLinkIdentity(ctx, q, cfg, external)
		if err != nil {
			return err
		}
		completed.Session, err = startLoginSession(ctx, q, cfg, user, external.DisplayName, invitation)
		if err != nil {
			return err
		}
		consumed, err := q.ConsumeMagicLink(ctx, db.ConsumeMagicLinkParams{
			ID:               link.ID,
			ConsumedByUserID: user.id,
		})
		if err != nil {
			return fmt.Errorf("consume magic link: %w", err)
		}
		if consumed == 0 {
			return ErrInvalidToken
		}
		completed.RedirectAfter = link.RedirectAfter.String
		return nil
	})
	if err != nil {
		return CompletedMagicLink{}, err
	}
	return completed, nil
}

// magicLinkIdentity is the identity a delivered magic link proves: ownership
// of its address.
func magicLinkIdentity(email string) ExternalIdentity {
	return ExternalIdentity{
		Provider:       magicLinkProvider,
		Subject:        email,
		DisplayName:    email,
		Email:          email,
		EmailVerified:  true,
		VerifiedEmails: []string{email},
	}
}

// upsertMagicLinkIdentity records a magic link identity. Unlike a provider
// identity, a delivered link verifies its address, which always becomes the
// user's primary email.
func upsertMagicLinkIdentity(ctx context.Context, q db.Querier, cfg Config, external ExternalIdentity) (signedInUser, error) {
	user, err := q.UpsertMagicLinkAuthIdentity(ctx, db.UpsertMagicLinkAuthIdentityParams{
		UserID:           pgvalue.UUID(uuid.NewV7()),
		IdentityID:       pgvalue.UUID(uuid.NewV7()),
		IdentityProvider: external.Provider,
		IdentitySubject:  external.Subject,
		DisplayName:      external.DisplayName,
		ProfileImageURL:  pgtype.Text{String: external.ProfileImageURL, Valid: external.ProfileImageURL != ""},
		Email:            pgtype.Text{String: external.Email, Valid: true},
		Admin:            cfg.initialAdmin(external.Email, true),
	})
	if err != nil {
		return signedInUser{}, fmt.Errorf("record magic link identity: %w", err)
	}
	return signedInUser{id: user.ID, disabled: user.DisabledAt.Valid}, nil
}
