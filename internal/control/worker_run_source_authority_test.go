package control

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestAuthorizeWorkerRunSourceRequiresWorkerAndFullReceipt(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*workerActor, *runLeaseClaimAuthorityFixture)
	}{
		{
			name: "wrong worker",
			mutate: func(worker *workerActor, _ *runLeaseClaimAuthorityFixture) {
				worker.WorkerGroupID = "other"
			},
		},
		{
			name: "wrong epoch",
			mutate: func(worker *workerActor, _ *runLeaseClaimAuthorityFixture) {
				worker.WorkerEpoch++
			},
		},
		{
			name: "altered full receipt",
			mutate: func(_ *workerActor, fixture *runLeaseClaimAuthorityFixture) {
				fixture.receipt.WriterGeneration++
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, store, worker, turnRequest, _ := newActorTurnCommitFixture(t)
			fixture := runLeaseClaimAuthorityFixture{
				receipt: turnRequest.Lease,
			}
			test.mutate(&worker, &fixture)
			_, err := authorizeWorkerRunSource(
				t.Context(), store, worker, fixture.receipt,
			)
			if !errors.Is(err, errStaleWorkerRunSource) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

type runLeaseClaimAuthorityFixture struct {
	receipt api.WorkerRunLeaseReceipt
}
