package session_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
)

// A revoked raw binding refuses new Sessions and input that needs a new
// process on the Computer; a live process keeps receiving input.
func TestRevokedRawBindingRefusesNewAdmission(t *testing.T) {
	f, e, store, secretID, _, _ := runtimeSecretsFixture(t)
	if _, err := store.Revoke(t.Context(), dbtest.SecretManager(t, f.pool, f.env), f.env, secretID, ""); err != nil {
		t.Fatal(err)
	}
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	start := session.StartRequest{EnvironmentID: f.env, Agent: "agent", ComputerID: f.computer, IdempotencyKey: "after-revoke", Input: json.RawMessage(`[{"type":"text","text":"new"}]`)}
	if _, err := session.Start(t.Context(), policy, f.pool, nil, f.caller(), start); !errors.Is(err, computer.ErrSecretUnavailable) {
		t.Fatalf("new Session on a Computer with a revoked raw binding: %v", err)
	}
	enqueue := func(key string) error {
		_, err := session.Enqueue(t.Context(), policy, f.pool, nil, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: e.SessionID, IdempotencyKey: key, Input: json.RawMessage(`[{"type":"text","text":"more"}]`)})
		return err
	}
	if err := enqueue("live-process"); err != nil {
		t.Fatalf("input for a live process refused: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID)
	if err := enqueue("needs-new-process"); !errors.Is(err, computer.ErrSecretUnavailable) {
		t.Fatalf("input needing a new process: %v", err)
	}
}

// A start refused for a revoked raw binding is terminal for that start: its
// queued input fails with secret_unavailable and the process gets stop intent.
// A later process for the Session is never assigned.
func TestRevokedRawBindingSettlesRefusedStart(t *testing.T) {
	original, allocator, e := startingSessionFixture(t)
	original.session = e.SessionID
	f, e, store, secretID, attachment, _ := runtimeSecretsOnFixture(t, original, e)
	if _, err := store.Revoke(t.Context(), dbtest.SecretManager(t, f.pool, f.env), f.env, secretID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := session.AuthorizeSessionStart(t.Context(), f.pool, *f.host(), e, attachment); !errors.Is(err, secret.ErrDeliveryRevoked) {
		t.Fatalf("start with a revoked raw binding: %v", err)
	}
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	for range 2 {
		if err := session.RefuseRevokedSessionStart(t.Context(), policy, f.pool, e); err != nil {
			t.Fatal(err)
		}
	}
	var failed, queued int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE status='failed' AND error_code='secret_unavailable'),count(*) FILTER (WHERE status='queued') FROM turns WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID).Scan(&failed, &queued); err != nil || failed == 0 || queued != 0 {
		t.Fatalf("refused start turns failed=%d queued=%d: %v", failed, queued, err)
	}
	var stopping bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='stopping' AND failure_recorded_at IS NOT NULL AND fenced_at IS NULL FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, f.env, e.SessionID, e.ProcessEpoch).Scan(&stopping); err != nil || !stopping {
		t.Fatalf("refused start did not stop the process: %v %v", stopping, err)
	}
	control, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), e, attachment)
	if err != nil || control.Kind != "shutdown" {
		t.Fatalf("refused start control: %+v %v", control, err)
	}
	var holds int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE session_id=$1`, e.SessionID).Scan(&holds); err != nil || holds != 0 {
		t.Fatalf("refused start created holds: %d %v", holds, err)
	}
	// After physical stop, queued input for a new process is failed, never assigned.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO turns(environment_id,id,session_id,computer_id,seq,caller_kind,caller_id,admission_method,target_id,idempotency_key,request_digest,input)
 SELECT environment_id,uuidv7(),id,computer_id,next_turn_seq,'user',uuidv7(),'enqueue',id,'late',decode(repeat('00',32),'hex'),convert_to('[]','UTF8') FROM sessions WHERE environment_id=$1 AND id=$2`, f.env, e.SessionID)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET next_turn_seq=next_turn_seq+1 WHERE environment_id=$1 AND id=$2`, f.env, e.SessionID)
	if p, err := allocator.AllocateSessionProcess(t.Context(), f.env, e.SessionID, e.ProcessEpoch+1); err == nil && p.Epoch != 0 {
		t.Fatalf("process assigned with a revoked raw binding: %+v", p)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE status='queued') FROM turns WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("queued input left waiting: %d %v", queued, err)
	}
}
