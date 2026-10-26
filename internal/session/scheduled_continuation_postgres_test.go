package session_test

import (
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

// Apply the promotion cutover with an empty definition set. The historical
// activation and its accepted Session remain pinned to the preceding deployment.
func omitScheduledAgent(t *testing.T, f fixture) {
	t.Helper()
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		if _, err := org.LockAdmission(t.Context(), tx, f.env); err != nil {
			return err
		}
		var cutover time.Time
		if err := tx.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&cutover); err != nil {
			return err
		}
		promoted := uuid.NewV7()
		if _, err := tx.Exec(t.Context(), `INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$2,'sha256:'||repeat('e',64))`, f.env, promoted); err != nil {
			return err
		}
		if err := agent.ReplaceActivations(t.Context(), tx, f.env, promoted, cutover, nil); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.env, promoted)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestScheduledSessionQueuedWorkSurvivesPromotion(t *testing.T) {
	f := newAdmissionFixture(t)
	trigger := seedTrigger(t, f, time.Now().UTC().Truncate(time.Minute))
	if err := agent.EvaluateTrigger(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.env, trigger); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT session_id FROM trigger_occurrences WHERE environment_id=$1 AND activation_id=$2 AND disposition='admitted'`, f.env, trigger).Scan(&id); err != nil {
		t.Fatal(err)
	}
	omitScheduledAgent(t, f)
	var allowed bool
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		var err error
		allowed, err = session.AuthorizeInitialSessions(t.Context(), auditpolicy.Policy{
			RetentionSeconds: 3600,
		}, tx, f.env, []uuid.UUID{id})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("promotion revoked an accepted scheduled Session before process admission")
	}
	var queued bool
	if err := f.pool.QueryRow(t.Context(), `SELECT s.deployment_id=$3 AND t.status='queued' AND t.error_code IS NULL FROM sessions s JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id) WHERE s.environment_id=$1 AND s.id=$2`, f.env, id, f.deployment).Scan(&queued); err != nil || !queued {
		t.Fatalf("promotion changed accepted work: queued=%v err=%v", queued, err)
	}
	if _, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: id, Input: json.RawMessage(`[{"type":"text","text":"continue"}]`), IdempotencyKey: "after-promotion"}); err != nil {
		t.Fatalf("existing Session could not accept another Turn: %v", err)
	}
	if _, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), f.startRequest("new-root")); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("absent Agent accepted an independent root: %v", err)
	}
}

func TestScheduledSessionCanSpawnAfterPromotion(t *testing.T) {
	f := newAdmissionFixture(t)
	trigger := seedTrigger(t, f, time.Now().UTC().Truncate(time.Minute))
	// Give the live runtime fixture the same immutable origin as a scheduled
	// admission, so the test can exercise child admission without a guest VM.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET origin_user_id=NULL,origin_activation_id=$3 WHERE environment_id=$1 AND id=$2`, f.env, f.session, trigger)
	admitted := f.enqueue(t, "scheduled-parent")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	omitScheduledAgent(t, f)
	caller := session.Caller{Kind: "session", MCPScopeID: f.mcpScope, ID: f.session, TurnID: admitted.TurnID, Execution: f.execution(), Host: f.host()}
	req := f.startRequest("scheduled-child")
	req.ComputerID = f.computer
	child, err := session.AdmitSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, req, "spawn")
	if err != nil {
		t.Fatalf("promotion revoked scheduled parent's child admission: %v", err)
	}
	var pinned bool
	if err := f.pool.QueryRow(t.Context(), `SELECT deployment_id=$3 AND origin_activation_id=$4 AND parent_session_id=$5 FROM sessions WHERE environment_id=$1 AND id=$2`, f.env, child.SessionID, f.deployment, trigger, f.session).Scan(&pinned); err != nil || !pinned {
		t.Fatalf("child lost version or origin: pinned=%v err=%v", pinned, err)
	}
}
