package agent

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestComputerSaveLossDistinguishesCommittedPublication(t *testing.T) {
	for _, stop := range []bool{false, true} {
		for _, state := range []string{"requested", "captured", "published"} {
			name := "expiry/" + state
			if stop {
				name = "physical-stop/" + state
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				turn, save := f.finalize(t, "prepared")
				queued := f.enqueue(t, "followup")
				storage := newSaveStorageFixture(t, f)
				cut, root := storage.cut(t, 3)
				if state != "requested" {
					f.capture(t, save, root)
					storage.certify(t, storage.ref(save.ID), cut)
				}
				if state == "published" {
					if err := storage.publisher.Publish(t.Context(), storage.ref(save.ID), cut, "committed before loss"); err != nil {
						t.Fatal(err)
					}
				}
				var pinsBefore int
				if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_object_pins WHERE save_id=$1`, save.ID).Scan(&pinsBefore); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					var err error
					if stop {
						err = ObserveComputerStopped(t.Context(), f.pool, *f.host(), leaseIdentity(f), uuid.Nil())
					} else {
						dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
						err = expireComputerLease(t.Context(), f.pool, f.env, f.computer, 1)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if state != "published" {
					for range 2 {
						if err := reconcileSessionCompletion(t.Context(), f.pool, f.env, f.session); err != nil {
							t.Fatal(err)
						}
					}
				}
				var saveState, turnState string
				if err := f.pool.QueryRow(t.Context(), `SELECT s.status,t.status FROM computer_saves s JOIN turns t ON t.id=s.turn_id AND t.environment_id=s.environment_id WHERE s.id=$1`, save.ID).Scan(&saveState, &turnState); err != nil {
					t.Fatal(err)
				}
				if state == "published" {
					if saveState != "published" || turnState != "finalizing" {
						t.Fatalf("committed receipt changed: %s %s", saveState, turnState)
					}
					if err := Complete(t.Context(), f.pool, f.env, f.session, turn.TurnID); err != nil {
						t.Fatal(err)
					}
				} else {
					if saveState != "failed" || turnState != "interrupted" {
						t.Fatalf("absence not settled: %s %s", saveState, turnState)
					}
					if err := storage.publisher.Publish(t.Context(), storage.ref(save.ID), cut, "late publication"); err == nil {
						t.Fatal("lost writer published fresh bytes")
					}
					if err := Complete(t.Context(), f.pool, f.env, f.session, turn.TurnID); !errors.Is(err, ErrTerminal) {
						t.Fatalf("absent save completed: %v", err)
					}
					var events int
					if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_events WHERE turn_id=$1 AND kind='turn.interrupted'`, turn.TurnID).Scan(&events); err != nil || events != 1 {
						t.Fatalf("interruption events %d %v", events, err)
					}
				}
				var queuedState string
				if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, queued.TurnID).Scan(&queuedState); err != nil || queuedState != "queued" {
					t.Fatalf("followup left queue after loss: %s %v", queuedState, err)
				}
				if _, err := Dispatch(t.Context(), f.pool, f.execution()); err == nil {
					t.Fatal("lost execution dispatched queued followup")
				}
				var pinsAfter int
				if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_object_pins WHERE save_id=$1`, save.ID).Scan(&pinsAfter); err != nil || pinsAfter != pinsBefore {
					t.Fatalf("loss bypassed retention owner: %d -> %d %v", pinsBefore, pinsAfter, err)
				}
			})
		}
	}
}

func TestComputerSaveLossPreservesCancellation(t *testing.T) {
	f := newFixture(t)
	turn, save := f.finalize(t, "cancelled-save")
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "cancel", "cancel-before-loss")); err != nil {
		t.Fatal(err)
	}
	if err := ObserveComputerStopped(t.Context(), f.pool, *f.host(), leaseIdentity(f), uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reconcileSessionLifecycle(t.Context(), f.pool, sessionLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, turn.TurnID).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("cancel outcome %s %v", state, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM computer_saves WHERE id=$1`, save.ID).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("save disposition %s %v", state, err)
	}
}

func TestComputerSaveLossSerializesWithPublication(t *testing.T) {
	for _, loss := range []string{"stop", "expiry", "expiry-no-member"} {
		for _, publishFirst := range []bool{false, true} {
			name := "stop-commits-first"
			if publishFirst {
				name = "publication-commits-first"
			}
			t.Run(loss+"/"+name, func(t *testing.T) {
				f := newFixture(t)
				turn, save := f.finalize(t, "racing-save")
				storage := newSaveStorageFixture(t, f)
				cut, root := storage.cut(t, 9)
				f.capture(t, save, root)
				ref := storage.ref(save.ID)
				storage.certify(t, ref, cut)
				if loss == "expiry-no-member" {
					dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp()`)
				}
				if loss != "stop" {
					if publishFirst {
						dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()+interval '2 seconds'`)
					} else {
						dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
					}
				}
				tx, err := f.pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(t.Context())
				first, err := NewSavePublisher(tx, storage.publisher.objects)
				if err != nil {
					t.Fatal(err)
				}
				endAuthority := func(pool db.TxBeginner) error {
					if loss == "stop" {
						return ObserveComputerStopped(t.Context(), pool, *f.host(), leaseIdentity(f), uuid.Nil())
					}
					return expireComputerLease(t.Context(), pool, f.env, f.computer, 1)
				}
				if publishFirst {
					err = first.Publish(t.Context(), ref, cut, "publication before stop")
				} else {
					err = endAuthority(tx)
				}
				if err != nil {
					t.Fatal(err)
				}
				if publishFirst && loss != "stop" {
					// Natural expiry adds no artificial lease-row write lock to
					// the production publication transaction being raced.
					dbtest.MustExec(t, t.Context(), f.pool, `SELECT pg_sleep(GREATEST(0,extract(epoch FROM expires_at-clock_timestamp()))) FROM computer_leases`)
				}
				done := make(chan error, 1)
				go func() {
					if publishFirst {
						done <- endAuthority(f.pool)
					} else {
						done <- storage.publisher.Publish(t.Context(), ref, cut, "publication after stop")
					}
				}()
				waitSessionLifecycleLock(t, f)
				if err := tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				err = <-done
				if publishFirst && err != nil {
					t.Fatal(err)
				}
				if !publishFirst && !errors.Is(err, ErrTerminal) && !errors.Is(err, ErrDenied) {
					t.Fatalf("fenced publication result: %v", err)
				}
				// Use the real lifecycle scan, including Sessions no longer in the live
				// Computer member set, rather than calling the settlement helper directly.
				for range 2 {
					if _, _, err := reconcileSessionLifecycle(t.Context(), f.pool, sessionLifecyclePosition{}); err != nil {
						t.Fatal(err)
					}
				}
				want := "interrupted"
				if publishFirst {
					want = "completed"
				}
				var status string
				if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, turn.TurnID).Scan(&status); err != nil || status != want {
					t.Fatalf("race outcome %s, want %s: %v", status, want, err)
				}
			})
		}
	}

}
