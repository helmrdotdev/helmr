package agent

import (
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func preparationResidentImage(t *testing.T) *preparationPublicationTest {
	t.Helper()
	p, key := newPreparationPublicationTest(t)
	root := p.capture(t, key)
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified"); err != nil {
		t.Fatal(err)
	}
	// Install the real published image through the admission operation; the
	// resident fixture represents the later allocator-created process and lease.
	dbtest.MustExec(t, t.Context(), p.f.pool, `UPDATE computers SET initial_root_id=NULL,initial_root_digest=NULL,preparation_spec_id=$3,preparation_deadline_at=clock_timestamp()+interval '5 minutes' WHERE environment_id=$1 AND id=$2`, p.f.env, p.f.computer, p.f.deployment)
	if _, err := PinComputerImage(t.Context(), p.f.pool, p.f.env, p.f.computer); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPreparationImageRevocationStopsPeersAndPreservesFinalization(t *testing.T) {
	p := preparationResidentImage(t)
	f := p.f.fixture
	peer := f.peer(t)
	running := f.enqueue(t, "running")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	queued := f.enqueue(t, "queued-behind-running")
	finalizing, _ := peer.finalize(t, "result-recorded")
	attachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	resume, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence)
	if err != nil || resume.Kind != "resume" {
		t.Fatalf("initial delivery: %v %v", resume, err)
	}
	if _, err = p.f.secrets.Revoke(t.Context(), f.env, p.f.secretID, "revoke"); err != nil {
		t.Fatal(err)
	}
	// These checks run before reconciliation, including the unchanged-control fast path.
	if _, err = Dispatch(t.Context(), f.pool, f.execution()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("revoked dispatch: %v", err)
	}
	if _, err = Enqueue(t.Context(), f.pool, f.caller(), EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, RetryKey: "after-revoke", Input: json.RawMessage(`[{"type":"text","text":"{}"}]`)}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("revoked enqueue: %v", err)
	}
	shutdown, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence)
	if err != nil || shutdown.Kind != "shutdown" || shutdown.Sequence <= resume.Sequence {
		t.Fatalf("shutdown: %v %v", shutdown, err)
	}
	if err = AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence, resume.Sequence, resume.Generation, resume.Kind, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale resume: %v", err)
	}
	if _, err = BeginComputerCapture(t.Context(), f.pool, *f.host(), f.captureRequest()); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked capture: %v", err)
	}
	for range 2 {
		if err = reconcileComputerImageRevocation(t.Context(), f.pool, f.env, f.computer); err != nil {
			t.Fatal(err)
		}
	}
	var stopped, holds int
	var unfenced bool
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*),bool_and(fenced_at IS NULL) FROM session_processes WHERE environment_id=$1 AND computer_id=$2 AND status='stopping'`, f.env, f.computer).Scan(&stopped, &unfenced); err != nil || stopped != 2 || !unfenced {
		t.Fatalf("peers %d unfenced %v: %v", stopped, unfenced, err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE environment_id=$1`, f.env).Scan(&holds); err != nil || holds != 2 {
		t.Fatalf("idempotent holds %d: %v", holds, err)
	}
	var status string
	if err = f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE environment_id=$1 AND id=$2`, f.env, running.TurnID).Scan(&status); err != nil || status != "interrupted" {
		t.Fatalf("running %s: %v", status, err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE environment_id=$1 AND id=$2`, f.env, queued.TurnID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("queued input %s: %v", status, err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE environment_id=$1 AND id=$2`, f.env, finalizing.TurnID).Scan(&status); err != nil || status != "finalizing" {
		t.Fatalf("recorded result %s: %v", status, err)
	}
	shutdown, err = PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence)
	if err != nil || shutdown.Kind != "shutdown" {
		t.Fatalf("control after reaper: %v %v", shutdown, err)
	}
	if err = AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence, shutdown.Sequence, shutdown.Generation, shutdown.Kind, ""); err != nil {
		t.Fatal(err)
	}
	if err = ObserveSessionStopped(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence); err != nil {
		t.Fatal(err)
	}
}

func TestPreparationImageRevocationLifecycleAndLease(t *testing.T) {
	p := preparationResidentImage(t)
	f := p.f.fixture
	var instanceID string
	if err := f.pool.QueryRow(t.Context(), `SELECT computer_instance_id::text FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, f.env, f.computer).Scan(&instanceID); err != nil {
		t.Fatal(err)
	}
	identity := ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, Epoch: 1}
	identity.InstanceID = uuid.MustParse(instanceID)
	if _, err := RenewComputerLease(t.Context(), f.pool, *f.host(), identity); err != nil {
		t.Fatal(err)
	}
	if _, err := p.f.secrets.Revoke(t.Context(), f.env, p.f.secretID, "revoke"); err != nil {
		t.Fatal(err)
	}
	if _, err := RenewComputerLease(t.Context(), f.pool, *f.host(), identity); err == nil {
		t.Fatal("revoked lease extended")
	}
	if _, _, err := reconcilePreparationLifecycle(t.Context(), f.pool, preparationLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	var pending bool
	if err := f.pool.QueryRow(t.Context(), `SELECT p.status='stopping' AND p.fenced_at IS NULL AND l.fenced_at IS NULL FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch) WHERE p.environment_id=$1 AND p.session_id=$2`, f.env, f.session).Scan(&pending); err != nil || !pending {
		t.Fatalf("physical ownership released: %v", err)
	}
}

// Bootstrap lineage on the authenticated initial root from the existing
// checkpoint fixture. Publication/key provenance is proved by the separate real
// capture tests; this fixture exercises the later restore/control transitions.
func checkpointImageExposure(t *testing.T, f fixture) preparationFixture {
	t.Helper()
	p := preparationFixtureFor(t, f)
	attempt := p.attach(t, p.waiter(t))
	ref := p.claim(t, attempt)
	if _, err := RecordPreparationExposure(t.Context(), f.pool, *f.host(), ref); err != nil {
		t.Fatal(err)
	}
	broker, _ := NewPreparationKeyBroker(f.pool, preparationWrapper(t))
	key, err := broker.WriteKey(t.Context(), *f.host(), ref)
	if err != nil {
		t.Fatal(err)
	}
	clear(key.Key)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_preparations p SET status='succeeded',logical_bytes=1048576,capture_root='sha256:'||encode(c.initial_root_digest,'hex'),capture_evidence='fixture authenticated root' FROM computers c WHERE p.environment_id=$1 AND p.id=$2 AND c.environment_id=$1 AND c.id=$3;
 INSERT INTO computer_images(environment_id,id,preparation_spec_id,preparation_id,seq,root_id,publication_evidence) SELECT $1,$2,$4,$2,1,initial_root_id,'fixture lineage' FROM computers WHERE environment_id=$1 AND id=$3;
 UPDATE computers SET preparation_spec_id=$4,image_id=$2 WHERE environment_id=$1 AND id=$3;`, pgx.QueryExecModeSimpleProtocol, f.env, attempt.ID, f.computer, f.deployment)
	return p
}

func TestPreparationImageRevocationDuringRestoreKeepsStopControl(t *testing.T) {
	for _, phase := range []string{"prepared", "consumed"} {
		t.Run(phase, func(t *testing.T) {
			r := newComputerRestoreFixture(t)
			secrets := checkpointImageExposure(t, r.f)
			request := r.prepare(t)
			if err := ValidateComputerRestore(t.Context(), r.f.pool, r.host, r.f.env, request, restoreReceipt(request, false, false)); err != nil {
				t.Fatal(err)
			}
			observed := restoreReceipt(request, true, false)
			if phase == "consumed" {
				if err := CommitComputerRestore(t.Context(), r.f.pool, r.host, r.f.env, request, observed); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := secrets.secrets.Revoke(t.Context(), r.f.env, secrets.secretID, "revoke"); err != nil {
				t.Fatal(err)
			}
			if err := ValidateComputerRestore(t.Context(), r.f.pool, r.host, r.f.env, request, observed); !errors.Is(err, ErrDenied) {
				t.Fatalf("validate revoked: %v", err)
			}
			err := CommitComputerRestore(t.Context(), r.f.pool, r.host, r.f.env, request, observed)
			if phase == "consumed" && err != nil || phase == "prepared" && !errors.Is(err, ErrDenied) {
				t.Fatalf("commit revoked %s: %v", phase, err)
			}
			attachment, err := AcquireRuntimeAttachment(t.Context(), r.f.pool, r.host, r.execution())
			if err != nil {
				t.Fatalf("stop transport: %v", err)
			}
			delivery, err := PrepareSessionDelivery(t.Context(), r.f.pool, r.host, r.execution(), attachment.Sequence)
			if err != nil || delivery.Kind != "shutdown" {
				t.Fatalf("restore shutdown: %v %v", delivery, err)
			}
			if err = reconcileComputerImageRevocation(t.Context(), r.f.pool, r.f.env, r.f.computer); err != nil {
				t.Fatal(err)
			}
			var state string
			if err = r.f.pool.QueryRow(t.Context(), `SELECT status FROM computer_checkpoints WHERE environment_id=$1 AND id=$2`, r.f.env, r.manifest.CheckpointID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			want := "restoring"
			if phase == "consumed" {
				want = "consumed"
			}
			if state != want {
				t.Fatalf("cleanup protocol lost: %s", state)
			}
			if phase == "consumed" {
				if err = CompleteComputerRestore(t.Context(), r.f.pool, r.host, r.f.env, request, restoreReceipt(request, true, true)); !errors.Is(err, ErrDenied) {
					t.Fatalf("activate revoked: %v", err)
				}
			}
		})
	}
}

func TestPreparationImageRevocationDeniesCheckpointProgress(t *testing.T) {
	f := newCheckpointStorageFixture(t)
	secrets := checkpointImageExposure(t, f.f)
	if _, err := secrets.secrets.Revoke(t.Context(), f.f.env, secrets.secretID, "revoke"); err != nil {
		t.Fatal(err)
	}
	if err := f.publisher.RegisterCheckpoint(t.Context(), f.ref, f.manifest); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked checkpoint registration: %v", err)
	}
}

func TestPreparationImageRevocationDeniesCaptureReplay(t *testing.T) {
	p := preparationResidentImage(t)
	req := p.f.captureRequest()
	if _, err := BeginComputerCapture(t.Context(), p.f.pool, *p.f.host(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := p.f.secrets.Revoke(t.Context(), p.f.env, p.f.secretID, "revoke"); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginComputerCapture(t.Context(), p.f.pool, *p.f.host(), req); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked capture replay: %v", err)
	}
}

func TestPreparationImageRevocationPreservesQueueWithoutProcess(t *testing.T) {
	p := preparationResidentImage(t)
	admission := p.f.enqueue(t, "queued")
	dbtest.MustExec(t, t.Context(), p.f.pool, `DELETE FROM session_processes WHERE environment_id=$1 AND session_id=$2`, p.f.env, p.f.session)
	if _, err := p.f.secrets.Revoke(t.Context(), p.f.env, p.f.secretID, "revoke"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := reconcileComputerImageRevocation(t.Context(), p.f.pool, p.f.env, p.f.computer); err != nil {
			t.Fatal(err)
		}
		next, more, err := reconcilePreparationLifecycle(t.Context(), p.f.pool, preparationLifecyclePosition{})
		if err != nil || more || next != (preparationLifecyclePosition{}) {
			t.Fatalf("retained queue scheduled unnecessary reconciliation: %+v %v %v", next, more, err)
		}
	}
	var retained bool
	var events, outstanding int
	if err := p.f.pool.QueryRow(t.Context(), `SELECT status='queued' AND terminal_at IS NULL AND processing_closed_at IS NULL AND input=$3::bytea FROM turns WHERE environment_id=$1 AND id=$2`, p.f.env, admission.TurnID, []byte(`[{"text":"{\"input\":1}","type":"text"}]`)).Scan(&retained); err != nil || !retained {
		t.Fatalf("queued input changed: %v", err)
	}
	if err := p.f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_events WHERE environment_id=$1 AND turn_id=$2 AND kind='turn.failed'`, p.f.env, admission.TurnID).Scan(&events); err != nil || events != 0 {
		t.Fatalf("unexpected failure events %d: %v", events, err)
	}
	if err := p.f.pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE environment_id=$1 AND status IN ('queued','running','finalizing')`, p.f.env).Scan(&outstanding); err != nil || outstanding != 1 {
		t.Fatalf("admission was released without disposition: %d %v", outstanding, err)
	}
	if _, err := ControlSession(t.Context(), p.f.pool, p.f.caller(), controlRequest(p.f.fixture, "cancel", "cancel-revoked-input")); err != nil {
		t.Fatalf("explicit cancellation of retained input: %v", err)
	}
	if err := p.f.pool.QueryRow(t.Context(), `SELECT status='cancelled' AND terminal_at IS NOT NULL FROM turns WHERE environment_id=$1 AND id=$2`, p.f.env, admission.TurnID).Scan(&retained); err != nil || !retained {
		t.Fatalf("explicit cancellation did not settle input: %v", err)
	}

}

func TestPreparationImageRevocationObserverBeforeReaper(t *testing.T) {
	for _, observer := range []string{"stop", "failure", "lease-expiry"} {
		t.Run(observer, func(t *testing.T) {
			p := preparationResidentImage(t)
			f := p.f.fixture
			a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = p.f.secrets.Revoke(t.Context(), f.env, p.f.secretID, "revoke"); err != nil {
				t.Fatal(err)
			}
			switch observer {
			case "stop":
				err = ObserveSessionStopped(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
			case "failure":
				err = ObserveSessionFailure(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
			case "lease-expiry":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
				err = expireComputerLease(t.Context(), f.pool, f.env, f.computer, 1)
			}
			if err != nil {
				t.Fatal(err)
			}
			var holds int
			if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE environment_id=$1 AND session_id=$2 AND reason='Computer exposed to a revoked Secret'`, f.env, f.session).Scan(&holds); err != nil || holds != 1 {
				t.Fatalf("observer explanation %d: %v", holds, err)
			}
			for range 2 {
				if err = reconcileComputerImageRevocation(t.Context(), f.pool, f.env, f.computer); err != nil {
					t.Fatal(err)
				}
			}
			if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE environment_id=$1 AND session_id=$2`, f.env, f.session).Scan(&holds); err != nil || holds != 1 {
				t.Fatalf("duplicate explanation %d: %v", holds, err)
			}
		})
	}
}

func TestPreparationImageRevocationSettlesHibernatedSource(t *testing.T) {
	r := newComputerRestoreFixture(t)
	secrets := checkpointImageExposure(t, r.f)
	if _, err := secrets.secrets.Revoke(t.Context(), r.f.env, secrets.secretID, "revoke"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reconcilePreparationLifecycle(t.Context(), r.f.pool, preparationLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	var inherited bool
	if err := r.f.pool.QueryRow(t.Context(), `SELECT p.status='lost' AND p.fenced_at=l.fenced_at AND p.failure_recorded_at IS NOT NULL FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch) WHERE p.environment_id=$1 AND p.session_id=$2`, r.f.env, r.f.session).Scan(&inherited); err != nil || !inherited {
		t.Fatalf("source physical evidence not retained: %v", err)
	}
	if _, err := PrepareComputerRestore(t.Context(), r.f.pool, r.host, r.f.env, r.manifest.CheckpointID, r.epoch, r.credential); !errors.Is(err, ErrNotReady) {
		t.Fatalf("revoked hibernation restore: %v", err)
	}
}
