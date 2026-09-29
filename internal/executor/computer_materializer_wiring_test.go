package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// unusedComputerRestoreControl satisfies the restore collaborator for mounts
// that do not restore a checkpoint; any call fails the mount.
type unusedComputerRestoreControl struct{}

func (unusedComputerRestoreControl) GetComputerRestorePlan(context.Context, workerapi.ComputerRestorePlanRequest) (workerapi.ComputerRestorePlanResponse, error) {
	return workerapi.ComputerRestorePlanResponse{}, errors.New("unexpected Computer restore plan request")
}

func (unusedComputerRestoreControl) AcknowledgeComputerRestore(context.Context, workerapi.ComputerRestoreAckRequest) (workerapi.ComputerRestoreAckResponse, error) {
	return workerapi.ComputerRestoreAckResponse{}, errors.New("unexpected Computer restore acknowledgement")
}

func completeTestComputerMaterializer() ComputerMaterializer {
	return ComputerMaterializer{
		RestoreControl:    unusedComputerRestoreControl{},
		ComputerSaves:     &saveHostFixture{},
		ComputerSaveEvery: time.Hour,
		CAS:               &fakeCAS{objects: map[string][]byte{}},
		ComputerObjects:   &checkpointCAS{},
		Sessions:          NewComputerMountSessions(),
		RuntimePool:       NewPreparedRuntimePool(nil, nil, 1, nil),
	}
}

func TestNewComputerMaterializerRejectsIncompleteWiring(t *testing.T) {
	complete := completeTestComputerMaterializer()
	if _, err := NewComputerMaterializer(complete); err != nil {
		t.Fatalf("NewComputerMaterializer(complete) error = %v", err)
	}
	for name, test := range map[string]struct {
		mutate func(*ComputerMaterializer)
		want   string
	}{
		"restore control":  {func(m *ComputerMaterializer) { m.RestoreControl = nil }, "Computer restore control plane is required"},
		"saves":            {func(m *ComputerMaterializer) { m.ComputerSaves = nil }, "Computer save control plane is required"},
		"save interval":    {func(m *ComputerMaterializer) { m.ComputerSaveEvery = 0 }, "Computer save interval must be positive"},
		"cas":              {func(m *ComputerMaterializer) { m.CAS = nil }, "computer materializer CAS is required"},
		"computer objects": {func(m *ComputerMaterializer) { m.ComputerObjects = nil }, "Computer object store is required"},
		"sessions":         {func(m *ComputerMaterializer) { m.Sessions = nil }, "computer mount session registry is required"},
		"runtime pool":     {func(m *ComputerMaterializer) { m.RuntimePool = nil }, "computer prepared runtime pool is required"},
	} {
		t.Run(name, func(t *testing.T) {
			materializer := complete
			test.mutate(&materializer)
			if _, err := NewComputerMaterializer(materializer); err == nil || err.Error() != test.want {
				t.Fatalf("NewComputerMaterializer() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRunComputerMountRejectsUnvalidatedMaterializer(t *testing.T) {
	withoutPool := completeTestComputerMaterializer()
	withoutPool.RuntimePool = nil
	for name, test := range map[string]struct {
		materializer ComputerMaterializer
		want         string
	}{
		"zero value":           {ComputerMaterializer{}, "Computer restore control plane is required"},
		"missing runtime pool": {withoutPool, "computer prepared runtime pool is required"},
	} {
		t.Run(name, func(t *testing.T) {
			_, mount := testComputerMountArtifacts(t)
			client := &computerMaterializerTestClient{}
			err := test.materializer.RunComputerMount(t.Context(), mount, client)
			if err == nil || err.Error() != "configure computer materializer: "+test.want {
				t.Fatalf("RunComputerMount() error = %v, want %q", err, test.want)
			}
			if len(client.renews) != 0 {
				t.Fatalf("renewals = %d, want 0", len(client.renews))
			}
			if len(client.failures) != 1 {
				t.Fatalf("failure reports = %d, want 1", len(client.failures))
			}
			var body struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(client.failures[0].Error, &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != "computer_mount_failed" || body.Message != test.want {
				t.Fatalf("failure report = %+v, want computer_mount_failed %q", body, test.want)
			}
		})
	}
}
