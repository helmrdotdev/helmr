package identity

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestMagicLinkPurposeContextAndDeletion(t *testing.T) {
	f := newIdentityFixture(t)
	orgID := f.organization(t, "challenge")
	other := f.organization(t, "challenge-other")
	invitation, _ := f.invitation(t, orgID, "join@example.test", "member")
	for _, tc := range []struct {
		purpose         string
		org, invitation any
		code            string
	}{
		{"invite_accept", nil, nil, "23514"},
		{"invite_accept", orgID, nil, "23514"},
		{"invite_accept", nil, invitation.ID, "23514"},
		{"login", orgID, nil, "23514"},
		{"login", orgID, invitation.ID, "23514"},
		{"login", nil, invitation.ID, "23514"},
		{"invite_accept", other, invitation.ID, "23503"},
	} {
		_, err := f.pool.Exec(t.Context(), `INSERT INTO magic_links(id,purpose,token_hash,email,org_id,invitation_id,expires_at) VALUES(uuidv7(),$1,$2,'join@example.test',$3,$4,clock_timestamp()+interval '1 hour')`, tc.purpose, []byte(uuid.NewV7().String()), tc.org, tc.invitation)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != tc.code {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	// A failed delivery retains both failure and revocation timestamps.
	link, _, err := CreateMagicLink(t.Context(), f.pool, f.cfg, invitationRecipient("join@example.test", orgID, uuid.UUID(invitation.ID.Bytes)), "")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := MarkMagicLinkDeliveryFailed(t.Context(), f.queries, link.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	var failed bool
	if err := f.pool.QueryRow(t.Context(), `SELECT delivery_failed_at IS NOT NULL AND revoked_at IS NOT NULL FROM magic_links WHERE id=$1`, link.ID).Scan(&failed); err != nil || !failed {
		t.Fatal(failed, err)
	}
	f.exec(t, `DELETE FROM invitations WHERE id=$1`, invitation.ID)
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM magic_links WHERE id=$1`, link.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	// Organization deletion cascades an invitation and its link together.
	invitation, _ = f.invitation(t, orgID, "cascade@example.test", "member")
	link, _, err = CreateMagicLink(t.Context(), f.pool, f.cfg, invitationRecipient("cascade@example.test", orgID, uuid.UUID(invitation.ID.Bytes)), "")
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `DELETE FROM organizations WHERE id=$1`, orgID)
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM magic_links WHERE id=$1`, link.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	// Historical consumed-user attribution is nullable after user deletion.
	login, _, err := CreateMagicLink(t.Context(), f.pool, f.cfg, MagicLinkRecipient{Purpose: db.MagicLinkPurposeLogin, Email: "historical@example.test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkMagicLinkSent(t.Context(), f.pool, login); err != nil {
		t.Fatal(err)
	}
	if _, err := CompleteMagicLink(t.Context(), f.pool, f.cfg, login.Token); err != nil {
		t.Fatal(err)
	}
	user, _ := f.userByEmail(t, "historical@example.test")
	f.exec(t, `DELETE FROM users WHERE id=$1`, user)
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT consumed_at IS NOT NULL AND consumed_by_user_id IS NULL FROM magic_links WHERE id=$1`, login.ID).Scan(&retained); err != nil || !retained {
		t.Fatal(retained, err)
	}
}

func TestDeviceCodeStateAndDeletedDecisionActor(t *testing.T) {
	f := newIdentityFixture(t)
	orgID := f.organization(t, "device-history")
	user := f.user(t, "Decider", "")
	member := f.member(t, orgID, user, "member")
	principal := auth.Principal{UserID: user, OrgID: orgID, MemberID: member, Kind: auth.PrincipalKindSession, SessionID: uuid.NewV7()}
	for _, set := range []string{
		"status='approved'", "status='denied'", "status='consumed'", "org_id=$2", "decided_at=now()", "consumed_at=now()",
		"status='approved',org_id=$2,decided_at=now(),consumed_at=now()",
		"status='denied',org_id=$2,decided_at=now(),consumed_at=now()",
		"status='consumed',org_id=$2,consumed_at=now()",
	} {
		id := uuid.NewV7()
		f.exec(t, `INSERT INTO device_codes(id,user_code_hash,device_code_hash,expires_at,poll_interval_seconds) VALUES($1,$2,$2,now()+interval '1 hour',5)`, id, []byte(id.String()))
		// Use both parameters even for a state mutation without an organization.
		_, err := f.pool.Exec(t.Context(), `UPDATE device_codes SET `+set+` WHERE id=$1 AND ($2::uuid IS NOT NULL)`, id, orgID)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
			t.Fatalf("%s: %v", set, err)
		}
	}
	var ids []uuid.UUID
	for _, state := range []string{"approved", "denied", "consumed"} {
		started, err := StartDeviceCode(t.Context(), f.pool, f.cfg)
		if err != nil {
			t.Fatal(err)
		}
		consent := DeviceConsent{UserID: user.String(), OrgID: orgID.String()}
		if state == "denied" {
			_, err = DenyDeviceCode(t.Context(), f.pool, f.cfg, principal, consent, started.UserCode)
		} else {
			_, err = ApproveDeviceCode(t.Context(), f.pool, f.cfg, principal, consent, started.UserCode)
		}
		if err != nil {
			t.Fatal(err)
		}
		if state == "consumed" {
			if _, err = ExchangeDeviceCode(t.Context(), f.pool, f.cfg, started.DeviceCode); err != nil {
				t.Fatal(err)
			}
		}
		hash, err := userCodeHash(f.cfg, started.UserCode)
		if err != nil {
			t.Fatal(err)
		}
		row, err := f.queries.GetDeviceCodeByUserCodeHash(t.Context(), hash)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, uuid.UUID(row.ID.Bytes))
	}
	f.exec(t, `DELETE FROM org_members WHERE id=$1`, member)
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*)=3 AND bool_and(decided_at IS NOT NULL AND org_id=$2 AND decided_by_member_id IS NULL AND ((status='consumed')=(consumed_at IS NOT NULL))) FROM device_codes WHERE id=ANY($1::uuid[])`, ids, orgID).Scan(&retained); err != nil || !retained {
		t.Fatal(retained, err)
	}
	f.exec(t, `DELETE FROM organizations WHERE id=$1`, orgID)
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM device_codes WHERE id=ANY($1::uuid[])`, ids).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
