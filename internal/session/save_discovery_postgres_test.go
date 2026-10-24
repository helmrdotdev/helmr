package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/dispatch"

	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func saveIdentity(f fixture) computer.LeaseIdentity {
	return computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, InstanceID: f.computer, Epoch: 1}
}

func TestBackgroundSaveAdmissionConvergesAndPreservesRequiredOrder(t *testing.T) {
	f := newFixture(t)
	identity := saveIdentity(f)
	if save, err := computer.NextSave(t.Context(), f.pool, *f.host(), identity, true, false, nil); err != nil || save != nil {
		t.Fatalf("not due: %+v %v", save, err)
	}
	const n = 8
	results := make(chan *computer.SaveRequest, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			s, e := computer.NextSave(ctx, f.pool, *f.host(), identity, true, true, nil)
			// Lock contention is retryable; every caller must still converge.
			for errors.Is(e, computer.ErrNotReady) && ctx.Err() == nil {
				s, e = computer.NextSave(ctx, f.pool, *f.host(), identity, true, true, nil)
			}
			results <- s
			errs <- e
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first *computer.SaveRequest
	for result := range results {
		if result == nil {
			t.Fatal("missing optional request")
		}
		if first == nil {
			first = result
		}
		if result.ID != first.ID || result.Sequence != 1 {
			t.Fatalf("duplicate optional admission: %+v", result)
		}
	}
	_, required := f.finalize(t, "required-behind-optional")
	if required.Sequence != 2 || required.ID == first.ID {
		t.Fatalf("required cut reused background: %+v", required)
	}
	if s, e := computer.NextSave(t.Context(), f.pool, *f.host(), identity, true, true, nil); e != nil || s == nil || s.ID != first.ID {
		t.Fatalf("uncertain admission displaced: %+v %v", s, e)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_saves SET status='failed',failure_evidence='fixture settlement' WHERE environment_id=$1 AND id=$2`, f.env, first.ID)
	if s, e := computer.NextSave(t.Context(), f.pool, *f.host(), identity, true, true, nil); e != nil || s == nil || s.ID != required.ID {
		t.Fatalf("required priority: %+v %v", s, e)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_saves WHERE environment_id=$1`, f.env).Scan(&count); err != nil || count != 2 {
		t.Fatalf("pending bound: %d %v", count, err)
	}
}

func TestBackgroundSavePublicationDoesNotCompleteRunningTurn(t *testing.T) {
	f := newFixture(t)
	storage := newSaveStorageFixture(t, f)
	admitted := f.enqueue(t, "running")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	save, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, true, nil)
	if err != nil || save == nil {
		t.Fatalf("optional: %+v %v", save, err)
	}
	root, _ := storage.cut(t, 17)
	if err := storage.publisher.Capture(t.Context(), storage.ref(save.ID), root, "optional cut"); err != nil {
		t.Fatal(err)
	}
	if err := storage.publish(t, save.ID, root); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT c.recovery_save_id=$2 AND s.status='published' AND s.turn_id IS NULL AND t.status='running' FROM computers c JOIN computer_saves s ON s.environment_id=c.environment_id AND s.id=$2 JOIN turns t ON t.environment_id=c.environment_id AND t.id=$3 WHERE c.environment_id=$1 AND c.id=$4`, f.env, save.ID, admitted.TurnID, f.computer).Scan(&valid); err != nil || !valid {
		t.Fatalf("background head/completion: %v %v", valid, err)
	}
	if err := completedTurnForTest(t.Context(), f.pool, f.env, f.session, admitted.TurnID); err == nil {
		t.Fatalf("background completed Turn: %v", err)
	}
	if err := closeProcessingForTest(t.Context(), f.pool, f.execution(), admitted.TurnID); err != nil {
		t.Fatal(err)
	}
	required, err := recordResultForTest(t.Context(), f.pool, f.execution(), admitted.TurnID, json.RawMessage(`{"ok":true}`), "work drained")
	if err != nil || required.ID == save.ID || required.Sequence != save.Sequence+1 {
		t.Fatalf("required own save: %+v %v", required, err)
	}
	ownRoot, _ := storage.cut(t, 19)
	if err := storage.publisher.Capture(t.Context(), storage.ref(required.ID), ownRoot, "required cut after result"); err != nil {
		t.Fatal(err)
	}
	if err := storage.publish(t, required.ID, ownRoot); err != nil {
		t.Fatal(err)
	}
	if err := completedTurnForTest(t.Context(), f.pool, f.env, f.session, admitted.TurnID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT status='completed' AND EXISTS(SELECT 1 FROM computer_saves s WHERE s.turn_id=turns.id AND s.id=$2) FROM turns WHERE id=$1`, admitted.TurnID, required.ID).Scan(&valid); err != nil || !valid {
		t.Fatalf("own-save completion: %v %v", valid, err)
	}
}

func TestBackgroundSaveDeclinesOptionalWorkButDiscoversPending(t *testing.T) {
	cases := map[string]string{
		"host-draining":  `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp(),drain_reason='admin' WHERE id=$1`,
		"group-paused":   `UPDATE worker_groups SET status='paused' WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`,
		"group-draining": `UPDATE worker_groups SET status='draining' WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`,
		"pool-draining":  `UPDATE worker_pools SET status='draining' WHERE id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1)`,
		"releasing":      `UPDATE computer_leases SET status='releasing' WHERE worker_host_id=$1`,
		"deleted":        `UPDATE computers SET deleted_at=clock_timestamp() WHERE id IN (SELECT computer_id FROM computer_leases WHERE worker_host_id=$1)`,
		"faulted":        `UPDATE computers SET integrity_fault_at=clock_timestamp(),integrity_fault_reason='fixture fault' WHERE id IN (SELECT computer_id FROM computer_leases WHERE worker_host_id=$1)`,
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			for _, pending := range []bool{false, true} {
				f := newFixture(t)
				var expected uuid.UUID
				if pending {
					_, save := f.finalize(t, "pending")
					expected = save.ID
				}
				dbtest.MustExec(t, t.Context(), f.pool, query, f.worker)
				save, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, true, nil)
				if err != nil {
					t.Fatal(err)
				}
				if pending {
					if save == nil || save.ID != expected {
						t.Fatalf("pending lost: %+v", save)
					}
				} else if save != nil {
					t.Fatalf("ineligible optional admitted: %+v", save)
				}
			}
		})
	}
}

func TestBackgroundSaveAndCheckpointAdmissionSerialize(t *testing.T) {
	for _, first := range []string{"save", "capture"} {
		t.Run(first+" first", func(t *testing.T) {
			f := newFixture(t)
			req := f.captureRequest()
			if control, err := session.ControlComputerExecution(t.Context(), f.pool, f.caller(), f.executionRequest("hibernate", req.CheckpointID.String())); err != nil || control.Status != "accepted" {
				t.Fatalf("idle hibernate intent: %+v %v", control, err)
			}
			var saveID uuid.UUID
			if first == "save" {
				hold, err := f.pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer hold.Rollback(t.Context())
				save, err := computer.NextSave(t.Context(), hold, *f.host(), saveIdentity(f), true, true, nil)
				if err != nil || save == nil || !save.Claimed {
					t.Fatalf("background save positive control: %+v %v", save, err)
				}
				saveID = save.ID
				var capture session.ComputerCapture
				err = commitWithBlockedOperation(t, f, hold, func(ctx context.Context) error {
					var err error
					capture, err = session.BeginComputerCapture(ctx, f.pool, *f.host(), req)
					return err
				})
				if !errors.Is(err, session.ErrNotReady) || capture.SaveID != uuid.Nil() || len(capture.Request) != 0 {
					t.Fatalf("checkpoint passed unresolved background save: %+v %v", capture, err)
				}
			} else {
				capture, err := session.BeginComputerCapture(t.Context(), f.pool, *f.host(), req)
				if err != nil || capture.SaveID == uuid.Nil() {
					t.Fatalf("idle capture positive control: %+v %v", capture, err)
				}
				saveID = capture.SaveID
				// The opposite order is a committed control: NextSave's short lock
				// timeout must not make this assertion depend on polling speed.
				if save, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, true, nil); err != nil || save != nil {
					t.Fatalf("background save bypassed checkpoint: %+v %v", save, err)
				}
			}
			var saves, checkpoints int
			var requested, owned bool
			if err := f.pool.QueryRow(t.Context(), `SELECT
 (SELECT count(*) FROM computer_saves WHERE environment_id=$1 AND computer_id=$2),
 (SELECT count(*) FROM checkpoints WHERE environment_id=$1 AND computer_id=$2),
 EXISTS(SELECT 1 FROM computer_saves WHERE environment_id=$1 AND computer_id=$2 AND id=$3 AND status='requested' AND turn_id IS NULL),
 EXISTS(SELECT 1 FROM checkpoints WHERE environment_id=$1 AND computer_id=$2 AND id=$4 AND disk_save_id=$3 AND status='capturing')`, f.env, f.computer, saveID, req.CheckpointID).Scan(&saves, &checkpoints, &requested, &owned); err != nil {
				t.Fatal(err)
			}
			wantCheckpoints := 0
			if first == "capture" {
				wantCheckpoints = 1
			}
			if saves != 1 || !requested || checkpoints != wantCheckpoints || owned != (first == "capture") {
				t.Fatalf("admission left wrong durable owner: saves=%d requested=%v checkpoints=%d owned=%v", saves, requested, checkpoints, owned)
			}
		})
	}
}

func TestBackgroundSaveRequiresExactLiveAuthority(t *testing.T) {
	for _, change := range []string{"environment", "computer", "instance", "epoch", "expiry", "fenced"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			identity := saveIdentity(f)
			switch change {
			case "environment":
				identity.EnvironmentID = uuid.NewV7()
			case "computer":
				identity.ComputerID = uuid.NewV7()
			case "instance":
				identity.InstanceID = uuid.NewV7()
			case "epoch":
				identity.Epoch++
			case "expiry":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1`, f.env)
			case "fenced":
				if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil(), uuid.Nil()); err != nil {
					t.Fatal(err)
				}
			}
			if save, err := computer.NextSave(t.Context(), f.pool, *f.host(), identity, true, true, nil); !errors.Is(err, computer.ErrDenied) || save != nil {
				t.Fatalf("invalid writer: %+v %v", save, err)
			}
		})
	}
}
func TestBackgroundSaveDeclinesRevokedImage(t *testing.T) {
	for _, pending := range []bool{false, true} {
		p := preparationResidentImage(t)
		f := p.f.fixture
		var expected uuid.UUID
		if pending {
			_, save := f.finalize(t, "pending")
			expected = save.ID
		}
		if _, err := p.f.secrets.Revoke(t.Context(), audit.System("test-fixture"), f.env, p.f.secretID, "revoke"); err != nil {
			t.Fatal(err)
		}
		save, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if pending {
			if save == nil || save.ID != expected {
				t.Fatalf("pending lost: %+v", save)
			}
		} else if save != nil {
			t.Fatalf("revoked optional: %+v", save)
		}
	}
}
func TestBackgroundSaveDiscoveryDoesNotLockComputerUntilDue(t *testing.T) {
	f := newFixture(t)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SELECT id FROM computers WHERE id=$1 FOR NO KEY UPDATE`, f.computer); err != nil {
		t.Fatal(err)
	}
	if save, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, false, nil); !errors.Is(err, computer.ErrNotReady) || save != nil {
		t.Fatalf("discovery bypassed lock: %+v %v", save, err)
	}
	if save, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, true, nil); !errors.Is(err, computer.ErrNotReady) || save != nil {
		t.Fatalf("optional admission bypassed lock: %+v %v", save, err)
	}
}
func TestBackgroundSaveLossInterruptsWorkWithoutCompletingIt(t *testing.T) {
	f := newFixture(t)
	turn := f.enqueue(t, "running")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	save, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, true, nil)
	if err != nil || save == nil {
		t.Fatalf("admission: %+v %v", save, err)
	}
	if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), saveIdentity(f), uuid.Nil(), uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT s.status='failed' AND s.turn_id IS NULL AND t.status='interrupted' FROM computer_saves s JOIN turns t ON t.environment_id=s.environment_id AND t.id=$2 WHERE s.id=$1`, save.ID, turn.TurnID).Scan(&valid); err != nil || !valid {
		t.Fatalf("loss disposition: %v %v", valid, err)
	}
}

func TestBackgroundSaveThenCheckpointKeepsCoherentRecoveryHead(t *testing.T) {
	f := newFixture(t)
	storage := newSaveStorageFixture(t, f)
	optional, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, true, nil)
	if err != nil || optional == nil {
		t.Fatalf("background admission: %+v %v", optional, err)
	}
	root, _ := storage.cut(t, 23)
	if err := storage.publisher.Capture(t.Context(), storage.ref(optional.ID), root, "optional before idle"); err != nil {
		t.Fatal(err)
	}
	if err := storage.publish(t, optional.ID, root); err != nil {
		t.Fatal(err)
	}
	checkpoint := checkpointStorageForSaveFixture(t, storage)
	if err := checkpoint.checkpoints.RegisterCheckpoint(t.Context(), checkpoint.ref, checkpoint.manifest); err != nil {
		t.Fatal(err)
	}
	checkpoint.upload(t)
	if err := checkpoint.publish(t, checkpoint.save, checkpoint.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.checkpoints.CompleteCheckpoint(t.Context(), checkpoint.ref, checkpoint.manifest); err != nil {
		t.Fatal(err)
	}
	if save, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, true, nil); err != nil || save != nil {
		t.Fatalf("optional passed ready checkpoint: %+v %v", save, err)
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT p.status='ready' AND c.recovery_save_id=p.disk_save_id AND p.disk_save_id<>$3 AND (SELECT count(*) FROM computer_saves s WHERE s.environment_id=c.environment_id AND s.computer_id=c.id)=2 FROM computers c JOIN checkpoints p ON p.environment_id=c.environment_id AND p.computer_id=c.id WHERE c.environment_id=$1 AND c.id=$2`, f.env, f.computer, optional.ID).Scan(&valid); err != nil || !valid {
		t.Fatalf("coherent checkpoint head: %v %v", valid, err)
	}
}

func TestAutosaveInspectionDoesNotFreezeCoverage(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	_, first := f.finalize(t, "first")
	inspect, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), false, false, nil)
	if err != nil || inspect == nil || inspect.Claimed || inspect.ID != first.ID || inspect.AgeMillis < 0 {
		t.Fatalf("metadata discovery: %+v %v", inspect, err)
	}
	var unfrozen bool
	if err = f.pool.QueryRow(t.Context(), `SELECT coverage_max_seq IS NULL FROM computer_saves WHERE id=$1`, first.ID).Scan(&unfrozen); err != nil || !unfrozen {
		t.Fatalf("inspection froze the prefix: %v %v", unfrozen, err)
	}
	_, second := peer.finalize(t, "later completion during quiet window")
	selected, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, false, nil)
	if err != nil || selected == nil || !selected.Claimed || selected.ID != first.ID {
		t.Fatalf("admission: %+v %v", selected, err)
	}
	var prefix int64
	if err = f.pool.QueryRow(t.Context(), `SELECT coverage_max_seq FROM computer_saves WHERE id=$1`, first.ID).Scan(&prefix); err != nil || prefix != second.Sequence {
		t.Fatalf("quiet-window completion excluded: %d %v", prefix, err)
	}
	// Losing an admission response must not create another batching wait or
	// expand the prefix, even when the retry only asks for metadata.
	retry, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), false, false, nil)
	if err != nil || retry == nil || !retry.Claimed || *retry != *selected {
		t.Fatalf("claimed prefix was hidden on retry: %+v %v", retry, err)
	}
}
