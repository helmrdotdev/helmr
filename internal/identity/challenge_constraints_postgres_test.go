package identity

import (
	"bytes"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"testing"
	"uuid"
)

func TestEmailChallengePurposeConstraints(t *testing.T) {
	f := newIdentityFixture(t)
	for _, purpose := range []string{"email_change", "unknown"} {
		_, err := f.pool.Exec(t.Context(), `INSERT INTO email_challenges(id,email,purpose,code_mac,next_send_at,browser_hash,expires_at) VALUES($1,'join@example.test',$2,$3,clock_timestamp(),$3,clock_timestamp()+interval '10 minutes')`, uuid.NewV7(), purpose, bytes.Repeat([]byte{1}, 32))
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "23514" {
			t.Fatal(purpose, err)
		}
	}
	c, b := emailChallenge(t, f, "history@example.test")
	if err := MarkEmailDelivered(t.Context(), f.pool, c); err != nil {
		t.Fatal(err)
	}
	if _, err := completeEmail(t, f, c, b); err != nil {
		t.Fatal(err)
	}
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT consumed_at IS NOT NULL FROM email_challenges WHERE id=$1`, c.ID).Scan(&retained); err != nil || !retained {
		t.Fatal(retained, err)
	}
}

func TestDeviceCodeStateAndDeletedDecisionActor(t *testing.T) {
	f := newIdentityFixture(t)
	orgID := f.organization(t, "device-history")
	user := f.user(t, "Decider", "")
	member := f.member(t, orgID, user, "member")
	principal, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, f.session(t, user, uuid.Nil()), orgID)
	if err != nil {
		t.Fatal(err)
	}
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
		if state == "denied" {
			_, err = DenyDeviceCode(t.Context(), f.pool, f.cfg, principal, started.UserCode)
		} else {
			_, err = ApproveDeviceCode(t.Context(), f.pool, f.cfg, principal, started.UserCode)
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
	// Captured credential provenance retains exact membership IDs. Logical removal
	// invalidates approval, while physical deletion cannot erase those references.
	f.exec(t, `UPDATE org_members SET removed_at=clock_timestamp() WHERE id=$1`, member)
	_, err = f.pool.Exec(t.Context(), `DELETE FROM org_members WHERE id=$1`, member)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23503" {
		t.Fatal("deleted retained authority", err)
	}
	var retained bool
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*)=3 AND bool_and(decided_at IS NOT NULL AND org_id=$2 AND decided_by_member_id=$3 AND ((status='consumed')=(consumed_at IS NOT NULL))) FROM device_codes WHERE id=ANY($1::uuid[])`, ids, orgID, member).Scan(&retained); err != nil || !retained {
		t.Fatal(retained, err)
	}
}

// No issued CLI Session may mask the pending approval's own membership fence.
func TestDeviceApprovalRequiresExactMemberBeforeExchange(t *testing.T) {
	f := newIdentityFixture(t)
	org := f.organization(t, "approval-only")
	user := f.user(t, "Approver", "")
	member := f.member(t, org, user, "member")
	principal, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, f.session(t, user, uuid.Nil()), org)
	if err != nil {
		t.Fatal(err)
	}
	started, err := StartDeviceCode(t.Context(), f.pool, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ApproveDeviceCode(t.Context(), f.pool, f.cfg, principal, started.UserCode); err != nil {
		t.Fatal(err)
	}
	hash, err := userCodeHash(f.cfg, started.UserCode)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{
		"decided_by_member_id=NULL",
		"status='consumed',consumed_at=clock_timestamp(),decided_by_member_id=NULL",
		"decided_by_member_id=gen_random_uuid()",
		"user_id=gen_random_uuid()",
		"org_id=gen_random_uuid()",
	} {
		_, err = f.pool.Exec(t.Context(), `UPDATE device_codes SET `+mutation+` WHERE user_code_hash=$1`, hash)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || (pe.Code != "23514" && pe.Code != "23503") {
			t.Fatalf("%s: %v", mutation, err)
		}
	}
	_, err = f.pool.Exec(t.Context(), `DELETE FROM org_members WHERE id=$1`, member)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23503" {
		t.Fatal("approval lost its member", err)
	}
	var memberFKs int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_constraint WHERE conrelid='device_codes'::regclass AND confrelid='org_members'::regclass AND contype='f'`).Scan(&memberFKs); err != nil || memberFKs != 1 {
		t.Fatal(memberFKs, err)
	}
	f.exec(t, `UPDATE org_members SET removed_at=clock_timestamp() WHERE id=$1`, member)
	if _, err = ExchangeDeviceCode(t.Context(), f.pool, f.cfg, started.DeviceCode); !errors.Is(err, ErrInvalidDeviceCode) {
		t.Fatal("removed member exchanged approval", err)
	}
}
