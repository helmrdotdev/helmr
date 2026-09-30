package run

import (
	"fmt"
	"reflect"
	"testing"

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
	for name, accessor := range map[string]func() any{
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
