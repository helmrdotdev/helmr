package identity

import (
	"errors"
	"fmt"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestMagicLinkPostgresLoginIsDeliveredAndConsumedOnce(t *testing.T) {
	fixture := newIdentityFixture(t)
	ctx := t.Context()
	recipient := MagicLinkRecipient{Purpose: db.MagicLinkPurposeLogin, Email: "admin@example.test"}

	older, created, err := CreateMagicLink(ctx, fixture.pool, fixture.cfg, recipient, "/auth/device?code=ABCD")
	if err != nil || !created {
		t.Fatalf("create = %t, %v", created, err)
	}
	if _, err := CompleteMagicLink(ctx, fixture.pool, fixture.cfg, older.Token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("undelivered link error = %v", err)
	}
	if err := MarkMagicLinkSent(ctx, fixture.pool, older); err != nil {
		t.Fatal(err)
	}
	if err := MarkMagicLinkSent(ctx, fixture.pool, older); err == nil {
		t.Fatal("a sent link was marked sent again")
	}
	newer, created, err := CreateMagicLink(ctx, fixture.pool, fixture.cfg, recipient, "/")
	if err != nil || !created {
		t.Fatalf("create newer = %t, %v", created, err)
	}
	if err := MarkMagicLinkSent(ctx, fixture.pool, newer); err != nil {
		t.Fatal(err)
	}
	if _, err := CompleteMagicLink(ctx, fixture.pool, fixture.cfg, older.Token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("superseded link error = %v", err)
	}

	completed, err := CompleteMagicLink(ctx, fixture.pool, fixture.cfg, newer.Token)
	if err != nil || completed.RedirectAfter != "/" {
		t.Fatalf("completed = %+v, err = %v", completed, err)
	}
	userID, admin := fixture.userByEmail(t, "admin@example.test")
	if !admin {
		t.Fatal("magic link address did not become an administrator")
	}
	principal, err := AuthenticateLoginSession(ctx, fixture.queries, fixture.cfg, completed.Session)
	if err != nil || principal.UserID != userID || principal.OrgID != uuid.Nil() {
		t.Fatalf("principal = %+v, err = %v", principal, err)
	}
	if _, err := CompleteMagicLink(ctx, fixture.pool, fixture.cfg, newer.Token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("reused link error = %v", err)
	}
	if _, err := CompleteMagicLink(ctx, fixture.pool, fixture.cfg, ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("empty link error = %v", err)
	}

	expiring, _, err := CreateMagicLink(ctx, fixture.pool, fixture.cfg, recipient, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkMagicLinkSent(ctx, fixture.pool, expiring); err != nil {
		t.Fatal(err)
	}
	fixture.expire(t, "magic_links", pgvalue.UUID(expiring.ID))
	if _, err := CompleteMagicLink(ctx, fixture.pool, fixture.cfg, expiring.Token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired link error = %v", err)
	}
}

func TestMagicLinkPostgresRateLimitsAndFailedDeliveries(t *testing.T) {
	fixture := newIdentityFixture(t)
	ctx := t.Context()
	recipient := MagicLinkRecipient{Purpose: db.MagicLinkPurposeLogin, Email: "limited@example.test"}

	failed, created, err := CreateMagicLink(ctx, fixture.pool, fixture.cfg, recipient, "")
	if err != nil || !created {
		t.Fatalf("create = %t, %v", created, err)
	}
	if marked, err := MarkMagicLinkDeliveryFailed(ctx, fixture.queries, failed.ID); err != nil || !marked {
		t.Fatalf("mark failed = %t, %v", marked, err)
	}
	if marked, err := MarkMagicLinkDeliveryFailed(ctx, fixture.queries, failed.ID); err != nil || marked {
		t.Fatalf("mark failed again = %t, %v", marked, err)
	}
	if err := MarkMagicLinkSent(ctx, fixture.pool, failed); err == nil {
		t.Fatal("a failed link was marked sent")
	}

	// A failed delivery does not count toward the limit.
	for index := range magicLinkRateLimitCount {
		if _, created, err := CreateMagicLink(ctx, fixture.pool, fixture.cfg, recipient, ""); err != nil || !created {
			t.Fatalf("create %d = %t, %v", index, created, err)
		}
	}
	if _, created, err := CreateMagicLink(ctx, fixture.pool, fixture.cfg, recipient, ""); err != nil || created {
		t.Fatalf("rate-limited create = %t, %v", created, err)
	}
	other := MagicLinkRecipient{Purpose: db.MagicLinkPurposeLogin, Email: "other@example.test"}
	if _, created, err := CreateMagicLink(ctx, fixture.pool, fixture.cfg, other, ""); err != nil || !created {
		t.Fatalf("other recipient create = %t, %v", created, err)
	}
}

func TestMagicLinkPostgresInvitationJoinsOrganization(t *testing.T) {
	fixture := newIdentityFixture(t)
	ctx := t.Context()
	orgID := fixture.organization(t, "magic")
	invitation, rawToken := fixture.invitation(t, orgID, "joiner@example.test", "admin")
	resolved, _, err := ResolveInvitation(ctx, fixture.queries, fixture.cfg, rawToken)
	if err != nil {
		t.Fatal(err)
	}
	link, _, err := CreateMagicLink(ctx, fixture.pool, fixture.cfg, invitationRecipient(resolved.InviteeEmail, resolved.OrgID, resolved.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkMagicLinkSent(ctx, fixture.pool, link); err != nil {
		t.Fatal(err)
	}
	completed, err := CompleteMagicLink(ctx, fixture.pool, fixture.cfg, link.Token)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := AuthenticateLoginSession(ctx, fixture.queries, fixture.cfg, completed.Session)
	if err != nil || principal.OrgID != orgID || principal.Role != auth.RoleAdmin {
		t.Fatalf("principal = %+v, err = %v", principal, err)
	}
	var consumedBy, acceptedBy uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `SELECT consumed_by_user_id FROM magic_links WHERE id = $1`, link.ID).Scan(&consumedBy); err != nil || consumedBy != principal.UserID {
		t.Fatalf("consumed by = %s, err = %v", consumedBy, err)
	}
	if err := fixture.pool.QueryRow(ctx, `SELECT accepted_by_user_id FROM invitations WHERE id = $1`, invitation.ID).Scan(&acceptedBy); err != nil || acceptedBy != principal.UserID {
		t.Fatalf("accepted by = %s, err = %v", acceptedBy, err)
	}

	// A link for an invitation revoked after delivery is not consumed.
	_, secondToken := fixture.invitation(t, orgID, "late@example.test", "viewer")
	second, _, err := ResolveInvitation(ctx, fixture.queries, fixture.cfg, secondToken)
	if err != nil {
		t.Fatal(err)
	}
	lateLink, _, err := CreateMagicLink(ctx, fixture.pool, fixture.cfg, invitationRecipient(second.InviteeEmail, second.OrgID, second.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkMagicLinkSent(ctx, fixture.pool, lateLink); err != nil {
		t.Fatal(err)
	}
	fixture.exec(t, `UPDATE invitations SET revoked_at = now() WHERE id = $1`, second.ID)
	if _, err := CompleteMagicLink(ctx, fixture.pool, fixture.cfg, lateLink.Token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked invitation link error = %v", err)
	}
	var consumed bool
	if err := fixture.pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM magic_links WHERE id = $1`, lateLink.ID).Scan(&consumed); err != nil || consumed {
		t.Fatalf("revoked invitation link consumed = %t, err = %v", consumed, err)
	}
}

func invitationRecipient(email string, orgID uuid.UUID, invitationID uuid.UUID) MagicLinkRecipient {
	return MagicLinkRecipient{Purpose: db.MagicLinkPurposeInviteAccept, Email: email, OrgID: orgID, InvitationID: invitationID}
}

func TestMagicLinkRecipientLockKeySeparatesRecipients(t *testing.T) {
	orgID := uuid.NewV7()
	recipients := []MagicLinkRecipient{
		{Purpose: db.MagicLinkPurposeLogin, Email: "a@example.test"},
		{Purpose: db.MagicLinkPurposeLogin, Email: "b@example.test"},
		invitationRecipient("a@example.test", orgID, uuid.NewV7()),
		invitationRecipient("a@example.test", orgID, uuid.NewV7()),
	}
	seen := map[int64]string{}
	for _, recipient := range recipients {
		key := magicLinkRecipientLockKey(recipient)
		if key < 0 || key != magicLinkRecipientLockKey(recipient) {
			t.Fatalf("lock key %d is negative or not deterministic", key)
		}
		description := fmt.Sprintf("%+v", recipient)
		if previous, ok := seen[key]; ok {
			t.Fatalf("recipients %s and %s share a lock key", previous, description)
		}
		seen[key] = description
	}
}
