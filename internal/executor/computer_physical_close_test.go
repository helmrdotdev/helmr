package executor

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestPhysicalCloseReportsCleanupOnlyAfterSuccessfulClose(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed", true: "close failed"}[fail], func(t *testing.T) {
			raw := &computerMaterializerTestSession{}
			if fail {
				raw.closeErr = errors.New("physical exclusion not established")
			}
			session := newManagedComputerMountSession(raw)
			client := &computerMaterializerTestClient{}
			err := (ComputerMaterializer{}).stopControlledComputerMount(t.Context(), session, workerapi.ComputerInstanceAssignment{ComputerInstanceID: "instance", RuntimeEpoch: 1, DesiredVersion: 2, ObservedVersion: 1}, client)
			if fail {
				if !errors.Is(err, raw.closeErr) || client.stops != 0 || len(client.failures) != 1 {
					t.Fatalf("err=%v stop=%d failure=%d", err, client.stops, len(client.failures))
				}
			} else if err != nil || client.stops != 1 || len(client.failures) != 0 {
				t.Fatalf("err=%v stop=%d failure=%d", err, client.stops, len(client.failures))
			}
			if raw.closeCount() != 1 {
				t.Fatalf("close count=%d", raw.closeCount())
			}
		})
	}
}
