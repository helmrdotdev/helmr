package run

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/secret"
)

func TestExecutionAccessorsReturnIsolatedCopies(t *testing.T) {
	var execution Execution
	dbtest.FillSlices(t, &execution.run)
	dbtest.FillSlices(t, &execution.attempt)
	dbtest.FillSlices(t, &execution.session)
	dbtest.FillSlices(t, &execution.lease)
	execution.secrets = make([]secret.DeliveryEnvelope, 2)
	for n := range execution.secrets {
		dbtest.FillSlices(t, &execution.secrets[n])
	}
	var turn LockedTurn
	dbtest.FillSlices(t, &turn.session)
	dbtest.FillSlices(t, &turn.turn)
	var controls ControlSecrets
	dbtest.FillSlices(t, &controls.target)
	controls.locked = make([]db.LockWorkerControlSecretsRow, 2)
	for n := range controls.locked {
		dbtest.FillSlices(t, &controls.locked[n])
		controls.locked[n].ComputerID = controls.target.ComputerID
	}
	for name, accessor := range map[string]func() any{
		"LockedTurn.Session":    func() any { return turn.Session() },
		"LockedTurn.Turn":       func() any { return turn.Turn() },
		"ControlSecrets.Target": func() any { return controls.Target() },
		"ControlSecrets.TargetBindings": func() any {
			return struct {
				Bindings []db.LockComputerSecretsForAdmissionRow
			}{controls.TargetBindings()}
		},
		"Run":             func() any { return execution.Run() },
		"Attempt":         func() any { return execution.Attempt() },
		"Session":         func() any { return execution.Session() },
		"Lease":           func() any { return execution.Lease() },
		"DeliverySecrets": func() any { return struct{ Secrets []secret.DeliveryEnvelope }{execution.DeliverySecrets()} },
	} {
		t.Run(name, func(t *testing.T) {
			// Snapshots are printed values, so they share no storage with the
			// execution.
			want := fmt.Sprintf("%#v", accessor())
			returned := reflect.New(reflect.TypeOf(accessor()))
			returned.Elem().Set(reflect.ValueOf(accessor()))
			dbtest.MutateSlices(t, returned.Interface())
			if fmt.Sprintf("%#v", returned.Elem().Interface()) == want {
				t.Fatal("mutation did not change the returned value")
			}
			if got := fmt.Sprintf("%#v", accessor()); got != want {
				t.Fatalf("mutating a returned value changed the next result:\ngot  %s\nwant %s", got, want)
			}
		})
	}
	if (Execution{}).DeliverySecrets() != nil {
		t.Fatal("an execution without Secret locks returned deliveries")
	}
}
