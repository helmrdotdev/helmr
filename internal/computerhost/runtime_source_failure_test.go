package computerhost

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"io/fs"
	"testing"
)

func TestRuntimeFailurePreservesSourceProvenance(t *testing.T) {
	target := workerapi.RuntimeReconcileTarget{ID: "runtime", WorkerEpoch: 3, DesiredVersion: 4, ObservedVersion: 2}
	for _, tc := range []struct {
		err  error
		code string
	}{
		{disk.PublishedSourceFailure(fs.ErrNotExist), workerapi.RuntimeFailureComputerSource},
		{&disk.DeviceFailure{Cause: fs.ErrNotExist}, workerapi.RuntimeFailureReconcile},
		{errors.New("temporary network failure"), workerapi.RuntimeFailureReconcile},
	} {
		got := runtimeTargetStatusRequest(target, tc.err)
		if got.ReasonCode != tc.code || got.WorkerEpoch != 3 || got.ExpectedObservedVersion != 2 || got.DesiredVersion != 4 {
			t.Fatalf("failure authority=%+v", got)
		}
	}
}
