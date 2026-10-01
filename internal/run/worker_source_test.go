package run

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestSourceReceiptFenceCarriesTheWorkerAndLease(t *testing.T) {
	worker := workergroup.HostPrincipal{HostID: uuid.NewV7(), GroupID: uuid.NewV7(), Epoch: 4, HostClaimVersion: 5, GroupClaimVersion: 6}
	leaseID := uuid.NewV7()
	fence, err := SourceReceipt{Worker: worker, LeaseID: leaseID.String(), LeaseSequence: 3}.Fence()
	if err != nil {
		t.Fatal(err)
	}
	want := ExecutionFence{LeaseID: pgvalue.UUID(leaseID), LeaseSequence: 3, WorkerGroupID: pgvalue.UUID(worker.GroupID), WorkerHostID: pgvalue.UUID(worker.HostID), WorkerEpoch: 4, GroupClaimVersion: 6, HostClaimVersion: 5}
	if fence != want {
		t.Fatalf("fence = %+v, want %+v", fence, want)
	}
}

func TestSourceReceiptFenceReportsAMalformedReceiptAsStale(t *testing.T) {
	for _, receipt := range []SourceReceipt{
		{LeaseID: "not-a-lease", LeaseSequence: 1},
		{LeaseID: uuid.NewV4().String(), LeaseSequence: 1},
		{LeaseID: uuid.NewV7().String(), LeaseSequence: 0},
		{LeaseID: uuid.NewV7().String(), LeaseSequence: -1},
	} {
		if _, err := receipt.Fence(); !errors.Is(err, ErrStaleSource) || err.Error() != "run source authority is stale: invalid receipt" {
			t.Fatalf("receipt %+v: err = %v", receipt, err)
		}
	}
}

// sourceTxBeginner fails Begin with its error, or begins a transaction that
// records how it ends and fails every statement.
type sourceTxBeginner struct {
	err error
	tx  *sourceTx
}

func (b sourceTxBeginner) Begin(context.Context) (pgx.Tx, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.tx, nil
}

type sourceTx struct {
	pgx.Tx
	committed, rolledBack bool
}

func (tx *sourceTx) Commit(context.Context) error   { tx.committed = true; return nil }
func (tx *sourceTx) Rollback(context.Context) error { tx.rolledBack = true; return nil }

func TestLockWorkerSourceChecksTheReceiptInsideItsTransaction(t *testing.T) {
	malformed := SourceReceipt{LeaseID: "not-a-lease", LeaseSequence: 1}
	unavailable := errors.New("database unavailable")
	if _, err := LockWorkerSource(t.Context(), sourceTxBeginner{err: unavailable}, malformed); !errors.Is(err, unavailable) || errors.Is(err, ErrStaleSource) {
		t.Fatalf("Begin failure with a malformed receipt = %v", err)
	}
	// The embedded nil transaction would panic on any statement, so this
	// passes only if the malformed receipt is reported before the source
	// lock runs.
	tx := &sourceTx{}
	if _, err := LockWorkerSource(t.Context(), sourceTxBeginner{tx: tx}, malformed); !errors.Is(err, ErrStaleSource) {
		t.Fatalf("malformed receipt = %v", err)
	}
	if tx.committed || !tx.rolledBack {
		t.Fatalf("malformed receipt transaction committed=%v rolled back=%v", tx.committed, tx.rolledBack)
	}
}
