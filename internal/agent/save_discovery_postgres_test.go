package agent

import (
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"sync"
	"testing"
	"uuid"
)

func saveIdentity(f fixture) ComputerLeaseIdentity {
	return ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, InstanceID: f.computer, Epoch: 1}
}

func TestBackgroundSaveAdmissionConvergesAndPreservesRequiredOrder(t *testing.T) {
	f := newFixture(t)
	identity := saveIdentity(f)
	if save, err := NextComputerSave(t.Context(), f.pool, *f.host(), identity, false); err != nil || save != nil {
		t.Fatalf("not due: %+v %v", save, err)
	}
	const n = 8
	results := make(chan *SaveRequest, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			s, e := NextComputerSave(t.Context(), f.pool, *f.host(), identity, true)
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
	var first *SaveRequest
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
	if s, e := NextComputerSave(t.Context(), f.pool, *f.host(), identity, true); e != nil || s == nil || s.ID != first.ID {
		t.Fatalf("uncertain admission displaced: %+v %v", s, e)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_saves SET status='failed',failure_evidence='fixture settlement' WHERE environment_id=$1 AND id=$2`, f.env, first.ID)
	if s, e := NextComputerSave(t.Context(), f.pool, *f.host(), identity, true); e != nil || s == nil || s.ID != required.ID {
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
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	save, err := NextComputerSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true)
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
	if err := f.pool.QueryRow(t.Context(), `SELECT c.recovery_save_id=$2 AND s.status='published' AND s.turn_id IS NULL AND t.status='running' AND t.completion_save_id IS NULL FROM computers c JOIN computer_saves s ON s.environment_id=c.environment_id AND s.id=$2 JOIN turns t ON t.environment_id=c.environment_id AND t.id=$3 WHERE c.environment_id=$1 AND c.id=$4`, f.env, save.ID, admitted.TurnID, f.computer).Scan(&valid); err != nil || !valid {
		t.Fatalf("background head/completion: %v %v", valid, err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, admitted.TurnID); err == nil {
		t.Fatalf("background completed Turn: %v", err)
	}
	if err := CloseProcessing(t.Context(), f.pool, f.execution(), admitted.TurnID); err != nil {
		t.Fatal(err)
	}
	required, err := RecordResult(t.Context(), f.pool, f.execution(), admitted.TurnID, json.RawMessage(`{"ok":true}`), "work drained")
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
	if err := Complete(t.Context(), f.pool, f.env, f.session, admitted.TurnID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT status='completed' AND completion_save_id=$2 FROM turns WHERE id=$1`, admitted.TurnID, required.ID).Scan(&valid); err != nil || !valid {
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
				save, err := NextComputerSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true)
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
	for range 6 {
		f := newFixture(t)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var save *SaveRequest
		var saveErr, captureErr error
		wg.Go(func() {
			<-start
			save, saveErr = NextComputerSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true)
		})
		wg.Go(func() {
			<-start
			_, captureErr = BeginComputerCapture(t.Context(), f.pool, *f.host(), f.captureRequest())
		})
		close(start)
		wg.Wait()
		if saveErr != nil {
			t.Fatal(saveErr)
		}
		if save != nil {
			if !errors.Is(captureErr, ErrNotReady) {
				t.Fatalf("checkpoint passed optional: %v", captureErr)
			}
		} else if captureErr != nil {
			t.Fatalf("neither admitted: %v", captureErr)
		}
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
				if err := ObserveComputerStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil()); err != nil {
					t.Fatal(err)
				}
			}
			if save, err := NextComputerSave(t.Context(), f.pool, *f.host(), identity, true); !errors.Is(err, ErrDenied) || save != nil {
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
		if _, err := p.f.secrets.Revoke(t.Context(), f.env, p.f.secretID, "revoke"); err != nil {
			t.Fatal(err)
		}
		save, err := NextComputerSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true)
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
	if save, err := NextComputerSave(t.Context(), f.pool, *f.host(), saveIdentity(f), false); err != nil || save != nil {
		t.Fatalf("ordinary discovery blocked: %+v %v", save, err)
	}
	if save, err := NextComputerSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true); !errors.Is(err, ErrNotReady) || save != nil {
		t.Fatalf("optional admission bypassed lock: %+v %v", save, err)
	}
}
func TestBackgroundSaveLossInterruptsWorkWithoutCompletingIt(t *testing.T) {
	f := newFixture(t)
	turn := f.enqueue(t, "running")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	save, err := NextComputerSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true)
	if err != nil || save == nil {
		t.Fatalf("admission: %+v %v", save, err)
	}
	if err := ObserveComputerStopped(t.Context(), f.pool, *f.host(), saveIdentity(f), uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT s.status='failed' AND s.turn_id IS NULL AND t.status='interrupted' AND t.completion_save_id IS NULL FROM computer_saves s JOIN turns t ON t.environment_id=s.environment_id AND t.id=$2 WHERE s.id=$1`, save.ID, turn.TurnID).Scan(&valid); err != nil || !valid {
		t.Fatalf("loss disposition: %v %v", valid, err)
	}
}

func TestBackgroundSaveThenCheckpointKeepsCoherentRecoveryHead(t *testing.T) {
	f := newFixture(t)
	storage := newSaveStorageFixture(t, f)
	optional, err := NextComputerSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true)
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
	if err := checkpoint.publisher.RegisterCheckpoint(t.Context(), checkpoint.ref, checkpoint.manifest); err != nil {
		t.Fatal(err)
	}
	checkpoint.upload(t)
	if err := checkpoint.publish(t, checkpoint.save, checkpoint.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.publisher.CompleteCheckpoint(t.Context(), checkpoint.ref, checkpoint.manifest); err != nil {
		t.Fatal(err)
	}
	if save, err := NextComputerSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true); err != nil || save != nil {
		t.Fatalf("optional passed ready checkpoint: %+v %v", save, err)
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT p.status='ready' AND c.recovery_save_id=p.disk_save_id AND p.disk_save_id<>$3 AND (SELECT count(*) FROM computer_saves s WHERE s.environment_id=c.environment_id AND s.computer_id=c.id)=2 FROM computers c JOIN computer_checkpoints p ON p.environment_id=c.environment_id AND p.computer_id=c.id WHERE c.environment_id=$1 AND c.id=$2`, f.env, f.computer, optional.ID).Scan(&valid); err != nil || !valid {
		t.Fatalf("coherent checkpoint head: %v %v", valid, err)
	}
}
