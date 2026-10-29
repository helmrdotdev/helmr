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

func TestQueuedHumanInputCannotAdoptNewProof(t *testing.T) {
	f := newFixture(t)
	pending := f.enqueue(t, "before-source-retirement")
	replacement := (sessiontest.Fixture{Pool: f.pool, User: f.user, Environment: f.env}).Principal(t)
	// Keep the browser envelope alive but replace its proof with another credential.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE browser_account_proofs SET revoked_at=clock_timestamp() WHERE session_id=$1`, f.human.SessionID)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO browser_account_proofs(id,session_id,user_id,identity_id,authenticated_at) VALUES($1,$2,$3,$4,clock_timestamp())`, uuid.NewV7(), f.human.SessionID, f.user, replacement.IdentityID)
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); !errors.Is(err, session.ErrNotReady) {
		t.Fatal("old input acquired replacement proof", err)
	}
	var status, code string
	if err := f.pool.QueryRow(t.Context(), `SELECT status,error_code FROM turns WHERE id=$1`, pending.TurnID).Scan(&status, &code); err != nil || status != "failed" || code != "permission_revoked" {
		t.Fatal(status, code, err)
	}
	current, err := access.SessionPrincipal(t.Context(), f.pool, f.human.SessionID, f.human.OrgID)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, session.UserCaller(current), session.EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, IdempotencyKey: "new-proof", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := dispatchTurnForTest(t.Context(), f.pool, f.execution())
	if err != nil || dispatched.TurnID != accepted.TurnID {
		t.Fatal(dispatched, err)
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
