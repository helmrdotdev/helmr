package session_test

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5"
	"testing"
	"uuid"
)

func TestQueuedHumanInputSurvivesRoutineAuthenticationChanges(t *testing.T) {
	for _, change := range []string{"logout", "expiry", "confirmation"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			pending := f.enqueue(t, "accepted-before-auth-change")
			switch change {
			case "logout":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE user_id=$1`, f.user)
			case "expiry":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE login_sessions SET expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1`, f.user)
			case "confirmation":
				replacement := (sessiontest.Fixture{Pool: f.pool, User: f.user, Environment: f.env}).Principal(t)
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE login_sessions SET identity_id=$2,authenticated_at=clock_timestamp() WHERE user_id=$1 AND channel='browser'`, f.user, replacement.IdentityID)
			}
			dispatched, err := dispatchTurnForTest(t.Context(), f.pool, f.execution())
			if err != nil || dispatched.TurnID != pending.TurnID {
				t.Fatal(dispatched, err)
			}
		})
	}
}

func TestQueuedHumanInputSurvivesAccessRevocation(t *testing.T) {
	for _, change := range []string{"account", "source", "member"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			pending := f.enqueue(t, "accepted-before-revocation")
			switch change {
			case "account":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE users SET access_epoch=access_epoch+1 WHERE id=$1`, f.user)
			case "source":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE auth_identities SET revoked_at=clock_timestamp() WHERE user_id=$1`, f.user)
			case "member":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE org_members SET reauthenticate_after=clock_timestamp() WHERE user_id=$1`, f.user)
			}
			result, err := dispatchTurnForTest(t.Context(), f.pool, f.execution())
			if err != nil || result.TurnID != pending.TurnID {
				t.Fatal(result, err)
			}

		})
	}
}

func TestCurrentParticipantCanContinueAfterOriginalSourceRetires(t *testing.T) {
	f := newFixture(t)
	var source uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT human_identity_id FROM sessions WHERE id=$1`, f.session).Scan(&source); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE auth_identities SET revoked_at=clock_timestamp() WHERE id=$1`, source)
	// A separate current login can read and continue organization-owned work.
	if _, err := session.GetSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, f.session); err != nil {
		t.Fatal("lost retained history", err)
	}
	if _, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, IdempotencyKey: "retired-origin", Input: json.RawMessage(`[]`)}); err != nil {
		t.Fatal("current participant could not continue", err)
	}
}

// Synthetic peer sessions retain the same source as their original runtime.
func copySessionHuman(t *testing.T, f fixture, target uuid.UUID) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET (`+access.HumanColumns+`)= (SELECT `+access.HumanColumns+` FROM sessions WHERE id=$1) WHERE id=$2`, f.session, target)
}
func TestRuntimeAgentScopeIsPublishedAndIndependentOfHumanSource(t *testing.T) {
	f := newFixture(t)
	caller := runtimeReadCaller(t, f)
	read := func() []byte {
		t.Helper()
		var raw []byte
		err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
			if err := session.LockRuntimeCaller(t.Context(), tx, caller); err != nil {
				return err
			}
			policy, err := session.RuntimeMCPPolicy(t.Context(), tx, caller)
			raw = policy.Bytes()
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := read()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE users SET access_epoch=access_epoch+1 WHERE id=$1; UPDATE auth_identities SET revoked_at=clock_timestamp() WHERE user_id=$1; DELETE FROM permission_group_members WHERE member_id IN (SELECT id FROM org_members WHERE user_id=$1)`, pgx.QueryExecModeSimpleProtocol, f.user)
	if string(read()) != string(before) {
		t.Fatal("published scope changed with requester authority")
	}
}
