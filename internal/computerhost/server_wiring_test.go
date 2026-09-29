package computerhost

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

func completeTestServer() Server {
	return Server{
		RestoreControl:    unusedComputerRestoreControl{},
		ComputerSaves:     &saveHostFixture{},
		ComputerSaveEvery: time.Hour,
		CAS:               &fakeCAS{objects: map[string][]byte{}},
		ComputerObjects:   &checkpointCAS{},
		Mounts:            NewMounts(),
		Machines:          NewPreparedMachines(nil, nil, 1, nil),
	}
}

func TestNewServerRejectsIncompleteWiring(t *testing.T) {
	complete := completeTestServer()
	if _, err := NewServer(complete); err != nil {
		t.Fatalf("NewServer(complete) error = %v", err)
	}
	for name, test := range map[string]struct {
		mutate func(*Server)
		want   string
	}{
		"restore control":  {func(m *Server) { m.RestoreControl = nil }, "Computer restore control plane is required"},
		"saves":            {func(m *Server) { m.ComputerSaves = nil }, "Computer save control plane is required"},
		"save interval":    {func(m *Server) { m.ComputerSaveEvery = 0 }, "Computer save interval must be positive"},
		"cas":              {func(m *Server) { m.CAS = nil }, "computer server CAS is required"},
		"computer objects": {func(m *Server) { m.ComputerObjects = nil }, "Computer object store is required"},
		"sessions":         {func(m *Server) { m.Mounts = nil }, "computer mount session registry is required"},
		"runtime machines": {func(m *Server) { m.Machines = nil }, "computer server prepared machines are required"},
	} {
		t.Run(name, func(t *testing.T) {
			server := complete
			test.mutate(&server)
			if _, err := NewServer(server); err == nil || err.Error() != test.want {
				t.Fatalf("NewServer() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestServeRejectsUnvalidatedServer(t *testing.T) {
	withoutMachines := completeTestServer()
	withoutMachines.Machines = nil
	for name, test := range map[string]struct {
		server Server
		want   string
	}{
		"zero value":               {Server{}, "Computer restore control plane is required"},
		"missing runtime machines": {withoutMachines, "computer server prepared machines are required"},
	} {
		t.Run(name, func(t *testing.T) {
			_, mount := testComputerMountArtifacts(t)
			client := &serverTestClient{}
			err := test.server.Serve(t.Context(), mount, client)
			if err == nil || err.Error() != "computer server is misconfigured: "+test.want {
				t.Fatalf("Serve() error = %v, want %q", err, test.want)
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
