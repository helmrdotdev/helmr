package computer

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestRunInstanceAccessorsReturnIsolatedCopies(t *testing.T) {
	var instance RunInstance
	dbtest.FillSlices(t, &instance.computer)
	dbtest.FillSlices(t, &instance.instance)
	for name, accessor := range map[string]func() any{
		"Computer": func() any { return instance.Computer() },
		"Instance": func() any { return instance.Instance() },
	} {
		t.Run(name, func(t *testing.T) {
			// Snapshots are printed values, so they share no storage with the
			// fence. The Computer row has no slice fields today, so only the
			// Instance mutation is required to change the returned value; a
			// new Computer slice field is filled and mutated here.
			want := fmt.Sprintf("%#v", accessor())
			returned := reflect.New(reflect.TypeOf(accessor()))
			returned.Elem().Set(reflect.ValueOf(accessor()))
			dbtest.MutateSlices(t, returned.Interface())
			if name == "Instance" && fmt.Sprintf("%#v", returned.Elem().Interface()) == want {
				t.Fatal("mutation did not change the returned value")
			}
			if got := fmt.Sprintf("%#v", accessor()); got != want {
				t.Fatalf("mutating a returned value changed the next result:\ngot  %s\nwant %s", got, want)
			}
		})
	}
}

func TestRunInstanceRecordStartRequiresALockedInstance(t *testing.T) {
	if err := (RunInstance{}).RecordStart(t.Context()); err == nil {
		t.Fatal("an unlocked Run Instance recorded a start")
	}
}

func TestSessionComputerAccessorReturnsIsolatedCopies(t *testing.T) {
	var locked SessionComputer
	dbtest.FillSlices(t, &locked.computer)
	want := fmt.Sprintf("%#v", locked.Computer())
	returned := locked.Computer()
	dbtest.MutateSlices(t, &returned)
	if fmt.Sprintf("%#v", returned) == want {
		t.Fatal("mutation did not change the returned value")
	}
	if got := fmt.Sprintf("%#v", locked.Computer()); got != want {
		t.Fatalf("mutating a returned value changed the next result:\ngot  %s\nwant %s", got, want)
	}
}
