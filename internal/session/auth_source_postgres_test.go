package session_test

import (
	"encoding/json"
	"errors"
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

func TestQueuedHumanInputStillRejectsIncidentRevocation(t *testing.T) {
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
			if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); !errors.Is(err, session.ErrNotReady) && !errors.Is(err, session.ErrDenied) {
				t.Fatal(err)
			}
			var failed bool
			if err := f.pool.QueryRow(t.Context(), `SELECT status='failed' AND error_code='permission_revoked' AND terminal_at IS NOT NULL FROM turns WHERE id=$1`, pending.TurnID).Scan(&failed); err != nil || !failed {
				t.Fatal(failed, err)
			}
		})
	}
}

func TestRuntimeOriginKeepsOriginalMembershipAndSource(t *testing.T) {
	f := newFixture(t)
	var source uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT human_identity_id FROM sessions WHERE id=$1`, f.session).Scan(&source); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE auth_identities SET revoked_at=clock_timestamp() WHERE id=$1`, source)
	// A separate current login still reads retained history, but cannot restart
	// execution whose original credential was retired.
	if _, err := session.GetSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, f.session); err != nil {
		t.Fatal("lost retained history", err)
	}
	if _, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, IdempotencyKey: "retired-origin", Input: json.RawMessage(`[]`)}); !errors.Is(err, session.ErrDenied) {
		t.Fatal("new login revived retired runtime source", err)
	}
}

// Synthetic peer sessions retain the same source as their original runtime.
func copySessionHuman(t *testing.T, f fixture, target uuid.UUID) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET (`+access.HumanColumns+`)= (SELECT `+access.HumanColumns+` FROM sessions WHERE id=$1) WHERE id=$2`, f.session, target)
}
func TestRuntimeAgentDiscoveryRetainsHumanSource(t *testing.T) {
	f := newFixture(t)
	caller := runtimeReadCaller(t, f)
	err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		all, _, err := session.RuntimeAgentScope(t.Context(), tx, caller)
		if err == nil && !all {
			t.Fatal("live user lost all-agent scope")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var source uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT human_identity_id FROM sessions WHERE id=$1`, f.session).Scan(&source); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE auth_identities SET revoked_at=clock_timestamp() WHERE id=$1`, source)
	err = db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		_, _, err := session.RuntimeAgentScope(t.Context(), tx, caller)
		return err
	})
	if !errors.Is(err, session.ErrDenied) {
		t.Fatal("retired source retained discovery", err)
	}
}
