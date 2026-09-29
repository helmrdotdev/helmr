package worker

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type runnerTestMaterializer struct {
	mounts []workerapi.ComputerInstanceAssignment
}

func (m *runnerTestMaterializer) RunComputerMount(_ context.Context, mount workerapi.ComputerInstanceAssignment, _ workerapi.ComputerMaterializerControlPlaneClient) error {
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
	materializer := &runnerTestMaterializer{}
	reservations := WithReservations(testRunnerReservations(t))
	if _, err := NewRunner(client, executor, materializer, workerapi.Capabilities{}, reservations); err != nil {
		t.Fatalf("NewRunner(complete) error = %v", err)
	}
	for name, test := range map[string]struct {
		client       ControlPlaneClient
		executor     RunLeaseExecutor
		materializer Materializer
		want         string
	}{
		"client":       {nil, executor, materializer, "worker client is required"},
		"executor":     {client, nil, materializer, "worker executor is required"},
		"materializer": {client, executor, nil, "worker materializer is required"},
	} {
		t.Run(name, func(t *testing.T) {
			runner, err := NewRunner(test.client, test.executor, test.materializer, workerapi.Capabilities{}, reservations)
			if runner != nil || err == nil || err.Error() != test.want {
				t.Fatalf("NewRunner() = %v, %v, want error %q", runner, err, test.want)
			}
		})
	}
}

func TestComputerConsumerRunsClaimedMountOnMaterializer(t *testing.T) {
	assignment := &workerapi.ComputerInstanceAssignment{ComputerInstanceID: "instance", WriterGeneration: 1}
	client := computerClaimTestClient{runConsumerTestClient: &runConsumerTestClient{}, assignment: assignment}
	materializer := &runnerTestMaterializer{}
	runner, err := NewRunner(client, &runConsumerTestExecutor{}, materializer, workerapi.Capabilities{}, WithReservations(testRunnerReservations(t)))
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
	if len(materializer.mounts) != 1 || materializer.mounts[0].ComputerInstanceID != "instance" {
		t.Fatalf("materialized mounts = %+v, want the claimed instance", materializer.mounts)
	}
}
