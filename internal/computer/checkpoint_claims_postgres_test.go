package computer_test

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// The checkpoint lifecycle carries no claim versions. Its worker authority is
// the locked host epoch and status: a claim bump or a drain does not stop a
// capture from beginning, registering, completing or failing, and a new epoch
// or a lost host does.
func TestCheckpointLifecycleFencesEpochAndStatusNotClaims(t *testing.T) {
	for _, transition := range hostTransitions {
		t.Run(transition.name+"/begin", func(t *testing.T) {
			f, _, _, capture := computertest.Capture(t)
			if err := transition.apply(t.Context(), f); err != nil {
				t.Fatal(err)
			}
			err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
				_, err := computer.BeginCapture(t.Context(), tx, capture)
				return err
			})
			if transition.rejected && !errors.Is(err, pgx.ErrNoRows) || !transition.rejected && err != nil {
				t.Fatalf("capture after %s: %v", transition.name, err)
			}
		})
		t.Run(transition.name+"/register", func(t *testing.T) {
			f, ref, manifest := computertest.RegisteredCapture(t, false)
			if err := transition.apply(t.Context(), f); err != nil {
				t.Fatal(err)
			}
			_, err := computer.RegisterCheckpoint(t.Context(), f.Pool, ref, manifest)
			requireLifecycleFence(t, transition.name, transition.rejected, err)
		})
		t.Run(transition.name+"/ready", func(t *testing.T) {
			f, ref, manifest, objects := computertest.ReadyCapture(t, false)
			if err := transition.apply(t.Context(), f); err != nil {
				t.Fatal(err)
			}
			_, err := completeCheckpoint(t, f, ref, manifest, objects)
			requireLifecycleFence(t, transition.name, transition.rejected, err)
		})
		t.Run(transition.name+"/abort", func(t *testing.T) {
			f, ref, _, key := captureAbortFixture(t, false)
			if err := transition.apply(t.Context(), f); err != nil {
				t.Fatal(err)
			}
			_, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
			requireLifecycleFence(t, transition.name, transition.rejected, err)
		})
	}
}

func requireLifecycleFence(t *testing.T, transition string, rejected bool, err error) {
	t.Helper()
	if rejected {
		if !errors.Is(err, computer.ErrAuthorityChanged) {
			t.Fatalf("after %s: %v", transition, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("after %s: %v", transition, err)
	}
}
