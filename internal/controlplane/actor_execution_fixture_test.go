package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

type actorExecutionFixture struct {
	runtest.Fixture
	server                               *Server
	worker                               workergroup.HostPrincipal
	computerID, rootID, sessionID, runID uuid.UUID
	claim                                run.Claim
	leaseID                              uuid.UUID
}

func newActorExecutionFixture(t *testing.T, input json.RawMessage, start bool) *actorExecutionFixture {
	t.Helper()
	base := runtest.New(t)
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE deployments SET queue_config='{"formatVersion":0,"queues":[{"concurrencyLimit":8,"name":"default"},{"name":"priority"}]}' WHERE id=$1`, base.DeploymentID)
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE worker_pools SET per_vm_guest_ephemeral_disk_bytes=34359738368,capacity_guest_ephemeral_disk_bytes=274877906944 WHERE id=$1`, base.WorkerPoolID)
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE worker_hosts SET per_vm_guest_ephemeral_disk_bytes=34359738368,epoch_guest_ephemeral_disk_bytes=274877906944 WHERE id=$1`, base.WorkerID)
	return actorExecutionOnFixture(t, base, input, start)
}

func actorExecutionOnFixture(t *testing.T, base runtest.Fixture, input json.RawMessage, start bool) *actorExecutionFixture {
	t.Helper()
	work := base.AddRunLease(t, "assigned", time.Now())
	sid := base.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE runs SET queue_concurrency_limit=8 WHERE id=$1`, work.RunID)
	manifest, digest, err := definition.CanonicalManifestAndDigest([]byte(`{"idleTimeoutMs":1000,"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE deployment_definitions SET manifest=$2,manifest_digest=$3 WHERE id=(SELECT deployment_definition_id FROM sessions WHERE id=$1)`, sid, manifest, digest[:])
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE sessions SET committed_input_sequence=0,next_input_sequence=1 WHERE id=$1`, sid)
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE runs SET session_input_start_sequence=0,session_input_high_watermark=0 WHERE id=$1`, work.RunID)
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE run_attempts SET session_input_start_sequence=0 WHERE run_id=$1`, work.RunID)
	f := &actorExecutionFixture{Fixture: base, sessionID: sid, runID: work.RunID, leaseID: work.LeaseID,
		server: &Server{db: db.New(base.Pool), tx: base.Pool, log: slog.Default()},
		worker: workergroup.HostPrincipal{HostID: base.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}}
	if err := base.Pool.QueryRow(t.Context(), `SELECT computer_id,base_computer_disk_version_id FROM runs WHERE id=$1`, work.RunID).Scan(&f.computerID, &f.rootID); err != nil {
		t.Fatal(err)
	}
	if input != nil {
		if _, err := session.ApplyAdmission(t.Context(), f.server.tx, session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: sid}, Mode: session.EnqueueOnly, Data: input}); err != nil {
			t.Fatal(err)
		}
	}
	if start {
		f.claimAndStart(t)
	}
	return f
}
func (f *actorExecutionFixture) claimLease(t *testing.T) {
	t.Helper()
	var err error
	f.claim, err = run.ClaimLease(t.Context(), f.Pool, f.executionFence(1))
	if err != nil {
		t.Fatal(err)
	}
}
func (f *actorExecutionFixture) claimAndStart(t *testing.T) {
	t.Helper()
	f.claimLease(t)
	f.startClaim(t)
}
func (f *actorExecutionFixture) startClaim(t *testing.T) {
	t.Helper()
	f.startLease(t, "actor", "test-actor")
}

// startLease starts the claimed lease and enters its entrypoint.
func (f *actorExecutionFixture) startLease(t *testing.T, kind, declaredID string) {
	t.Helper()
	fence := f.executionFence(f.claim.Lease().LeaseSequence)
	if err := run.StartLease(t.Context(), f.Pool, fence); err != nil {
		t.Fatal(err)
	}
	if err := run.EnterEntrypoint(t.Context(), f.Pool, fence, kind, declaredID); err != nil {
		t.Fatal(err)
	}
}

// executionFence is the fixture worker's fence on its lease.
func (f *actorExecutionFixture) executionFence(sequence int64) run.ExecutionFence {
	return run.ExecutionFence{LeaseID: pgvalue.UUID(f.leaseID), LeaseSequence: sequence, WorkerGroupID: pgvalue.UUID(f.worker.GroupID), WorkerHostID: pgvalue.UUID(f.worker.HostID), WorkerEpoch: f.worker.Epoch, GroupClaimVersion: f.worker.GroupClaimVersion, HostClaimVersion: f.worker.HostClaimVersion}
}
func (f *actorExecutionFixture) workerCall(t *testing.T, handler http.HandlerFunc, body any, result any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	r = r.WithContext(context.WithValue(t.Context(), workerContextKey{}, f.worker))
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("Worker request %T: status=%d body=%s", body, w.Code, w.Body.String())
	}
	if result != nil {
		if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *actorExecutionFixture) fence() workerapi.RunLeaseFence {
	lease := f.claim.Lease()
	return workerapi.RunLeaseFence{ID: pgvalue.UUIDString(lease.ID), LeaseSequence: lease.LeaseSequence}
}

func (f *actorExecutionFixture) receiveTurn(t *testing.T, sequence int64) run.TurnScope {
	t.Helper()
	var input db.SessionTurn
	input, err := f.server.db.GetSessionTurnAtSequenceForUpdate(t.Context(), db.GetSessionTurnAtSequenceForUpdateParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), SessionID: pgvalue.UUID(f.sessionID), Sequence: sequence})
	if err != nil {
		t.Fatal(err)
	}
	if input.Status == "queued" {
		after := sequence - 1
		params, _ := json.Marshal(workerActorInputWaitParams{SessionID: f.sessionID.String(), AfterInputSequence: after})
		f.workerCall(t, f.server.workerCreateRunWait, workerapi.CreateRunWaitRequest{CorrelationID: uuid.NewV7().String(), Lease: f.fence(), RunWaitID: uuid.NewV7().String(), ResumeAttachID: uuid.NewV7().String(), Kind: "actor_input", Params: params, ActorSpeculativeInputSequence: &after}, nil)
		input, err = f.server.db.GetSessionTurnAtSequenceForUpdate(t.Context(), db.GetSessionTurnAtSequenceForUpdateParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), SessionID: pgvalue.UUID(f.sessionID), Sequence: sequence})
		if err != nil {
			t.Fatal(err)
		}
	}
	if input.Status != "running" || !input.RunGeneration.Valid {
		t.Fatalf("input was not activated: %+v", input)
	}
	return run.TurnScope{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID, TurnID: pgvalue.MustUUIDValue(input.ID), RunID: f.runID, AttemptNumber: input.AttemptNumber.Int32, RunGeneration: input.RunGeneration.Int64}
}

func (f *actorExecutionFixture) turn(t *testing.T, sequence int64) workerapi.CommitActorTurnResponse {
	t.Helper()
	var headBefore, baseBefore uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT w.head_disk_version_id,a.base_computer_disk_version_id FROM computers w JOIN run_leases l ON l.computer_id=w.id JOIN run_attempts a ON a.run_id=l.run_id AND a.number=l.attempt_number WHERE w.id=$1 AND l.id=$2`, f.computerID, f.claim.Lease().ID).Scan(&headBefore, &baseBefore); err != nil {
		t.Fatal(err)
	}
	scope := f.receiveTurn(t, sequence)
	f.beginSettlement(t, scope)
	req := workerapi.CommitActorTurnRequest{TurnID: scope.TurnID.String(), RunGeneration: scope.RunGeneration, Disposition: "completed", Result: json.RawMessage(`null`), Lease: f.fence(), CorrelationID: uuid.NewV7().String(), TargetInputSequence: sequence}
	parsed, err := parseActorTurnCommitRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.server.commitActorTurn(t.Context(), f.worker, req, parsed)
	if err != nil {
		t.Fatalf("commit Turn %d: %v", sequence, err)
	}
	replayed, err := f.server.commitActorTurn(t.Context(), f.worker, req, parsed)
	if err != nil || replayed != out {
		t.Fatalf("Turn replay: %+v %v", replayed, err)
	}
	conflicting := req
	conflicting.Result = json.RawMessage(`{"different":true}`)
	conflictingParsed, err := parseActorTurnCommitRequest(conflicting)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.commitActorTurn(t.Context(), f.worker, conflicting, conflictingParsed); !errors.Is(err, errStaleActorTurnCommit) {
		t.Fatalf("conflicting replay: %v", err)
	}

	var head, base uuid.UUID
	var cursor int64
	var version pgtype.UUID
	var data []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT w.head_disk_version_id,a.base_computer_disk_version_id,s.committed_input_sequence,e.computer_disk_version_id,e.data FROM computers w JOIN run_leases l ON l.computer_id=w.id JOIN run_attempts a ON a.run_id=l.run_id AND a.number=l.attempt_number JOIN sessions s ON s.id=$3 JOIN session_events e ON e.id=$4 WHERE w.id=$1 AND l.id=$2`, f.computerID, f.claim.Lease().ID, f.sessionID, uuid.MustParse(out.EventID)).Scan(&head, &base, &cursor, &version, &data); err != nil {
		t.Fatal(err)
	}
	if head != headBefore || base != baseBefore || cursor != sequence || version.Valid || strings.Contains(string(data), "computer_disk_version_id") {
		t.Fatalf("Turn changed persistence or failed to advance cursor: head=%s base=%s cursor=%d version=%v data=%s", head, base, cursor, version, data)
	}
	return out
}
