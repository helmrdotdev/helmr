package computerhost

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"io/fs"
	"testing"
)

func TestInstanceFailurePreservesSourceProvenance(t *testing.T) {
	target := workerapi.InstanceReconcileTarget{ID: "instance", WorkerEpoch: 3, DesiredVersion: 4, ObservedVersion: 2}
	for _, tc := range []struct {
		err  error
		code string
	}{
		{disk.PublishedSourceFailure(fs.ErrNotExist), workerapi.InstanceFailureComputerSource},
		{&disk.DeviceFailure{Cause: fs.ErrNotExist}, workerapi.InstanceFailureReconcile},
		{errors.New("temporary network failure"), workerapi.InstanceFailureReconcile},
	} {
		got := instanceTargetStatusRequest(target, tc.err)
		if got.ReasonCode != tc.code || got.WorkerEpoch != 3 || got.ExpectedObservedVersion != 2 || got.DesiredVersion != 4 {
			t.Fatalf("failure authority=%+v", got)
		}
	}
}
