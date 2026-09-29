package worker

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type runnerTestComputerServer struct {
	mounts []workerapi.ComputerInstanceAssignment
}

func (m *runnerTestComputerServer) Serve(_ context.Context, mount workerapi.ComputerInstanceAssignment, _ workerapi.ComputerServerControlPlaneClient) error {
	m.mounts = append(m.mounts, mount)
	return nil
}

type computerClaimTestClient struct {
	*runConsumerTestClient
	assignment *workerapi.ComputerInstanceAssignment
}

func (c computerClaimTestClient) ClaimComputerInstance(context.Context) (workerapi.ComputerInstanceClaimResponse, error) {
	return workerapi.ComputerInstanceClaimResponse{Assignment: c.assignment}, nil
}

func testRunnerReservations(t *testing.T) *reservation.Ledger {
	t.Helper()
	ledger, err := reservation.New(reservation.Vector{CPUMillis: 1000, MemoryBytes: 1 << 30, VMSlots: 1})
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func TestNewRunnerRejectsMissingCollaborators(t *testing.T) {
	client := &runConsumerTestClient{}
	executor := &runConsumerTestExecutor{}
	computerServer := &runnerTestComputerServer{}
	reservations := WithReservations(testRunnerReservations(t))
	if _, err := NewRunner(client, executor, computerServer, workerapi.Capabilities{}, reservations); err != nil {
		t.Fatalf("NewRunner(complete) error = %v", err)
	}
	for name, test := range map[string]struct {
		client         ControlPlaneClient
		executor       RunLeaseExecutor
		computerServer ComputerServer
		want           string
	}{
		"client":         {nil, executor, computerServer, "worker client is required"},
		"executor":       {client, nil, computerServer, "worker executor is required"},
		"computerServer": {client, executor, nil, "worker Computer server is required"},
	} {
		t.Run(name, func(t *testing.T) {
			runner, err := NewRunner(test.client, test.executor, test.computerServer, workerapi.Capabilities{}, reservations)
			if runner != nil || err == nil || err.Error() != test.want {
				t.Fatalf("NewRunner() = %v, %v, want error %q", runner, err, test.want)
			}
		})
	}
}

func TestComputerConsumerServesClaimedMount(t *testing.T) {
	assignment := &workerapi.ComputerInstanceAssignment{ComputerInstanceID: "instance", WriterGeneration: 1}
	client := computerClaimTestClient{runConsumerTestClient: &runConsumerTestClient{}, assignment: assignment}
	computerServer := &runnerTestComputerServer{}
	runner, err := NewRunner(client, &runConsumerTestExecutor{}, computerServer, workerapi.Capabilities{}, WithReservations(testRunnerReservations(t)))
	if err != nil {
		t.Fatal(err)
	}
	work, ok, err := NewComputerConsumer(runner).Claim(t.Context())
	if err != nil || !ok || work == nil {
		t.Fatalf("Claim() = (%v, %t, %v), want work", work, ok, err)
	}
	if err := work(t.Context()); err != nil {
		t.Fatalf("run claimed mount: %v", err)
	}
	if len(computerServer.mounts) != 1 || computerServer.mounts[0].ComputerInstanceID != "instance" {
		t.Fatalf("served mounts = %+v, want the claimed instance", computerServer.mounts)
	}
}
