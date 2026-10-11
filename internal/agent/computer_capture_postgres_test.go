package agent

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"
)

func (f fixture) captureRequest() ComputerCaptureRequest {
	return ComputerCaptureRequest{EnvironmentID: f.env, ComputerID: f.computer, CheckpointID: uuid.NewV7(), LeaseEpoch: 1, ChannelCredential: agenttest.ChannelCredential}
}
func beginCapture(t *testing.T, f fixture, req ComputerCaptureRequest) (ComputerCapture, *agentv1.ComputerSessionCapture) {
	t.Helper()
	result, err := BeginComputerCapture(t.Context(), f.pool, *f.host(), req)
	if err != nil {
		t.Fatal(err)
	}
	request := new(agentv1.ComputerSessionCapture)
	if err = proto.Unmarshal(result.Request, request); err != nil {
		t.Fatal(err)
	}
	return result, request
}

func TestComputerCaptureSealsAllMembersAndReplaysExactSecretRequest(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	req := f.captureRequest()
	result, request := beginCapture(t, f, req)
	if len(request.Sessions) != 2 || request.Envelope.ChannelCredential != agenttest.ChannelCredential || request.Envelope.ComputerInstanceId != f.computer.String() {
		t.Fatalf("capture membership/owner differs")
	}
	retry, err := BeginComputerCapture(t.Context(), f.pool, *f.host(), req)
	if err != nil || !bytes.Equal(retry.Request, result.Request) || retry.SaveID != result.SaveID {
		t.Fatalf("uncertain retry changed capture: %v", err)
	}
	for _, member := range []fixture{f, peer} {
		admitted := member.enqueue(t, uuid.NewV7().String())
		if admitted.TurnID == uuid.Nil() {
			t.Fatal("public queue rejected")
		}
		if _, err = Dispatch(t.Context(), f.pool, member.execution()); !errors.Is(err, ErrNotReady) {
			t.Fatalf("dispatch bypassed seal: %v", err)
		}
		if _, err = AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), member.execution()); err != nil {
			t.Fatalf("capture blocked control attachment: %v", err)
		}
	}
	caller := Caller{Kind: "session", ID: f.session, Execution: f.execution(), Host: f.host()}
	if _, err = Enqueue(t.Context(), f.pool, caller, EnqueueRequest{EnvironmentID: f.env, SessionID: peer.session, RetryKey: "from-sealed-session", Input: []byte(`[]`)}); !errors.Is(err, ErrDenied) {
		t.Fatalf("sealed guest admitted side effect: %v", err)
	}
	other := req
	other.CheckpointID = uuid.NewV7()
	if _, err = BeginComputerCapture(t.Context(), f.pool, *f.host(), other); !errors.Is(err, ErrNotReady) {
		t.Fatalf("second capture = %v", err)
	}
	req.ChannelCredential = "wrong"
	if _, err = BeginComputerCapture(t.Context(), f.pool, *f.host(), req); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong credential obtained secret capture: %v", err)
	}
}

func TestComputerCaptureRacesDispatch(t *testing.T) {
	for range 4 {
		f := newFixture(t)
		f.enqueue(t, "queued")
		start := make(chan struct{})
		var group sync.WaitGroup
		var captureErr, dispatchErr error
		group.Go(func() {
			<-start
			_, captureErr = BeginComputerCapture(t.Context(), f.pool, *f.host(), f.captureRequest())
		})
		group.Go(func() { <-start; _, dispatchErr = Dispatch(t.Context(), f.pool, f.execution()) })
		close(start)
		group.Wait()
		if (captureErr == nil) == (dispatchErr == nil) {
			t.Fatalf("expected exactly one winner: capture=%v dispatch=%v", captureErr, dispatchErr)
		}
		loser := captureErr
		if loser == nil {
			loser = dispatchErr
		}
		if !errors.Is(loser, ErrNotReady) {
			t.Fatal(loser)
		}
	}
}

func TestComputerCaptureNeverSealedCancellationAndVersion(t *testing.T) {
	f := newFixture(t)
	req := f.captureRequest()
	result, request := beginCapture(t, f, req)
	early := UnsealedCaptureEvidence{AbsentObservedAt: time.Now()}
	if err := CancelUnsealedComputerCapture(t.Context(), f.pool, *f.host(), f.env, req.CheckpointID, early); !errors.Is(err, ErrNotReady) {
		t.Fatalf("early absence unsealed dispatch: %v", err)
	}
	rejected := UnsealedCaptureEvidence{Rejected: true}
	for range 2 {
		if err := CancelUnsealedComputerCapture(t.Context(), f.pool, *f.host(), f.env, req.CheckpointID, rejected); err != nil {
			t.Fatal(err)
		}
	}
	if replay, err := BeginComputerCapture(t.Context(), f.pool, *f.host(), req); !errors.Is(err, ErrNotReady) || len(replay.Request) != 0 || replay.SaveID != uuid.Nil() {
		t.Fatalf("cancelled capture reissued an executable envelope: %v", err)
	}
	var state string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM computer_saves WHERE id=$1`, result.SaveID).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("uncaptured save: %s %v", state, err)
	}
	req.CheckpointID = uuid.NewV7()
	_, next := beginCapture(t, f, req)
	if next.DesiredVersion <= request.DesiredVersion {
		t.Fatal("capture reused control version")
	}
}

func TestComputerCaptureSealedCannotBeCancelledAsAbsent(t *testing.T) {
	f := newFixture(t)
	req := f.captureRequest()
	_, request := beginCapture(t, f, req)
	receipt := &agentv1.ComputerSessionReceipt{CheckpointId: req.CheckpointID.String(), DesiredVersion: request.DesiredVersion, Frozen: true, ActivationStarted: true}
	if err := RecordComputerSealed(t.Context(), f.pool, *f.host(), f.env, req.CheckpointID, receipt); !errors.Is(err, ErrConflict) {
		t.Fatalf("activated receipt accepted: %v", err)
	}
	receipt.ActivationStarted = false
	for range 2 {
		if err := RecordComputerSealed(t.Context(), f.pool, *f.host(), f.env, req.CheckpointID, receipt); err != nil {
			t.Fatal(err)
		}
	}
	if err := CancelUnsealedComputerCapture(t.Context(), f.pool, *f.host(), f.env, req.CheckpointID, UnsealedCaptureEvidence{Rejected: true}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("sealed capture discarded: %v", err)
	}
}

func TestComputerCaptureSourceLossRequiresFenceAndRetainsHolds(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	req := f.captureRequest()
	beginCapture(t, f, req)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
	if err := LoseUnreadyComputerCapture(t.Context(), f.pool, f.env, req.CheckpointID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("bearer expiry treated as physical fence: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='VMM exited'`)
	for range 2 {
		if err := LoseUnreadyComputerCapture(t.Context(), f.pool, f.env, req.CheckpointID); err != nil {
			t.Fatal(err)
		}
	}
	for _, member := range []fixture{f, peer} {
		var holds, generation int
		if err := f.pool.QueryRow(t.Context(), `SELECT authority_generation,(SELECT count(*) FROM session_holds WHERE session_id=s.id AND released_at IS NULL) FROM sessions s WHERE id=$1`, member.session).Scan(&generation, &holds); err != nil {
			t.Fatal(err)
		}
		if holds != 1 || generation != 2 {
			t.Fatalf("loss duplicated or omitted control: holds=%d generation=%d", holds, generation)
		}
	}
}

func TestComputerCaptureRejectsActivePeerWithoutAllocatingSave(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	peer.enqueue(t, "active")
	if _, err := Dispatch(t.Context(), f.pool, peer.execution()); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginComputerCapture(t.Context(), f.pool, *f.host(), f.captureRequest()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("active peer captured: %v", err)
	}
	var saves, checkpoints int
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM computer_saves),(SELECT count(*) FROM computer_checkpoints)`).Scan(&saves, &checkpoints); err != nil {
		t.Fatal(err)
	}
	if saves != 0 || checkpoints != 0 {
		t.Fatal("failed eligibility left a partial save/seal")
	}
}

func TestComputerCaptureAbsentEvidenceMustFollowExpiry(t *testing.T) {
	f := newFixture(t)
	req := f.captureRequest()
	_, request := beginCapture(t, f, req)
	// Age the complete immutable fixture together, without waiting out a bearer.
	expired := time.Now().Add(-time.Minute)
	request.Envelope.OperationExpiresAtUnixNano = expired.UnixNano()
	raw, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_checkpoints SET capture_expires_at=$2,capture_request=$3 WHERE id=$1`, req.CheckpointID, expired, raw)
	if err = CancelUnsealedComputerCapture(t.Context(), f.pool, *f.host(), f.env, req.CheckpointID, UnsealedCaptureEvidence{AbsentObservedAt: expired.Add(-time.Second)}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("late delivery of old absence accepted: %v", err)
	}
	if err = CancelUnsealedComputerCapture(t.Context(), f.pool, *f.host(), f.env, req.CheckpointID, UnsealedCaptureEvidence{AbsentObservedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, "after-cancel")
	if _, err = Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatalf("never-sealed source did not resume dispatch: %v", err)
	}
}

func TestComputerCaptureContradictoryDiskCutKeepsSeal(t *testing.T) {
	f := newFixture(t)
	req := f.captureRequest()
	result, _ := beginCapture(t, f, req)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_saves SET status='captured',captured_root_digest=decode(repeat('1',64),'hex'),capture_evidence='cut',flush_acknowledged_at=clock_timestamp(),captured_at=clock_timestamp() WHERE id=$1`, result.SaveID)
	if err := CancelUnsealedComputerCapture(t.Context(), f.pool, *f.host(), f.env, req.CheckpointID, UnsealedCaptureEvidence{Rejected: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("disk cut was silently discarded: %v", err)
	}
	var state string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM computer_checkpoints WHERE id=$1`, req.CheckpointID).Scan(&state); err != nil || state != "capturing" {
		t.Fatalf("seal changed: %s %v", state, err)
	}
}

func TestComputerCaptureRejectsStaleProcessLease(t *testing.T) {
	f := newFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='source closed'`)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,status,computer_instance_id,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
  SELECT environment_id,computer_id,2,worker_host_id,worker_epoch,clock_timestamp()+interval '1 hour','active',$1,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE epoch=1`, uuid.NewV7())
	req := f.captureRequest()
	req.LeaseEpoch = 2
	if result, err := BeginComputerCapture(t.Context(), f.pool, *f.host(), req); !errors.Is(err, ErrNotReady) || len(result.Request) != 0 {
		t.Fatalf("stale process placement admitted: %v", err)
	}
}

func TestComputerCaptureUnsealedCancelAllowsMemberFencing(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	req := f.captureRequest()
	beginCapture(t, f, req)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1`, peer.session)
	if err := CancelUnsealedComputerCapture(t.Context(), f.pool, *f.host(), f.env, req.CheckpointID, UnsealedCaptureEvidence{Rejected: true}); err != nil {
		t.Fatalf("membership drift trapped never-sealed source: %v", err)
	}
	var scrubbed bool
	if err := f.pool.QueryRow(t.Context(), `SELECT capture_request IS NULL FROM computer_checkpoints WHERE id=$1`, req.CheckpointID).Scan(&scrubbed); err != nil || !scrubbed {
		t.Fatalf("cancelled source secret retained: %v", err)
	}
	f.enqueue(t, "healthy-peer")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatalf("healthy member cannot continue: %v", err)
	}
}

func TestComputerCaptureLossPreservesUnacknowledgedAndRecordedCuts(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(map[bool]string{false: "unacknowledged", true: "recorded"}[recorded], func(t *testing.T) {
			f := newFixture(t)
			req := f.captureRequest()
			result, _ := beginCapture(t, f, req)
			root := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
			if recorded {
				if err := RecordCapture(t.Context(), f.pool, CaptureEvidence{EnvironmentID: f.env, SaveID: result.SaveID, LeaseEpoch: 1, DiskRoot: root, Evidence: "frozen cut"}); err != nil {
					t.Fatal(err)
				}
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='VMM exited'`)
			if err := LoseUnreadyComputerCapture(t.Context(), f.pool, f.env, req.CheckpointID); err != nil {
				t.Fatal(err)
			}
			var state string
			var scrubbed bool
			if err := f.pool.QueryRow(t.Context(), `SELECT s.status,c.capture_request IS NULL FROM computer_saves s JOIN computer_checkpoints c ON c.disk_save_id=s.id WHERE s.id=$1`, result.SaveID).Scan(&state, &scrubbed); err != nil {
				t.Fatal(err)
			}
			want := "requested"
			if recorded {
				want = "captured"
			}
			if state != want || !scrubbed {
				t.Fatalf("lost source state: save=%s scrubbed=%v", state, scrubbed)
			}
			if !recorded {
				if err := RecordCapture(t.Context(), f.pool, CaptureEvidence{EnvironmentID: f.env, SaveID: result.SaveID, LeaseEpoch: 1, DiskRoot: root, Evidence: "recovered original cut after lost acknowledgement"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := ReconcileSavePublication(t.Context(), f.pool, f.env, result.SaveID, root); !errors.Is(err, ErrNotReady) {
				t.Fatalf("uncommitted cut became published after source loss: %v", err)
			}
			var noHead bool
			if err := f.pool.QueryRow(t.Context(), `SELECT recovery_save_id IS NULL FROM computers WHERE id=$1`, f.computer).Scan(&noHead); err != nil || !noHead {
				t.Fatalf("unknown publication advanced head: %v", err)
			}

		})
	}
}

func TestComputerCaptureRejectsCrossComputerStorageAndMembers(t *testing.T) {
	f := newFixture(t)
	other := uuid.NewV7()
	session := uuid.NewV7()
	save := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `
 INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest) SELECT environment_id,$2,initial_root_id,initial_root_digest FROM computers WHERE id=$1;
 INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,computer_instance_id,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
 SELECT environment_id,$2,epoch,worker_host_id,worker_epoch,expires_at,$3,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE computer_id=$1;
 INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth)
 SELECT 'until_environment_deletion',environment_id,$4,agent_id,deployment_id,$2,$4,0 FROM sessions WHERE id=$5;
 INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status)
 SELECT environment_id,$4,1,$2,1,'ready' FROM sessions WHERE id=$5;
 INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq)
 SELECT environment_id,$6,$2,1,1 FROM computers WHERE id=$1;`, pgx.QueryExecModeSimpleProtocol, f.computer, other, uuid.NewV7(), session, f.session, save)
	req := f.captureRequest()
	beginCapture(t, f, req)
	for _, tc := range []struct {
		name, statement string
		args            []any
	}{
		{"save", `UPDATE computer_checkpoints SET disk_save_id=$3 WHERE environment_id=$1 AND id=$2`, []any{f.env, req.CheckpointID, save}},
		{"process", `INSERT INTO computer_checkpoint_members(environment_id,computer_id,checkpoint_id,session_id,process_epoch) VALUES($1,$2,$3,$4,1)`, []any{f.env, f.computer, req.CheckpointID, session}},
		{"checkpoint", `INSERT INTO computer_checkpoint_members(environment_id,computer_id,checkpoint_id,session_id,process_epoch) VALUES($1,$2,$3,$4,1)`, []any{f.env, other, req.CheckpointID, session}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.pool.Exec(t.Context(), tc.statement, tc.args...)
			var fk *pgconn.PgError
			if !errors.As(err, &fk) || fk.Code != "23503" {
				t.Fatalf("cross-Computer relation accepted or wrong error: %v", err)
			}
		})
	}
}
