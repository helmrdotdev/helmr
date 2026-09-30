package controlplane

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleRunLeaseClaim = errors.New("run lease claim is stale")

type runLeaseClaimAuthority struct {
	resumeWait *db.RunWait
	actor      db.Session
	run        db.Run
	computer   db.LockRunLeaseClaimComputerRow
	attempt    db.RunAttempt
	runtime    db.ComputerInstance
	runLease   db.RunLease
}

func (s *Server) claimRunLease(ctx context.Context, worker workergroup.HostPrincipal, leaseID pgtype.UUID, leaseSequence int64) (runLeaseClaimAuthority, []secret.DeliveryEnvelope, error) {
	fence := run.ExecutionFence{LeaseID: leaseID, LeaseSequence: leaseSequence, WorkerGroupID: pgvalue.UUID(worker.GroupID), WorkerHostID: pgvalue.UUID(worker.HostID), WorkerEpoch: worker.Epoch, GroupClaimVersion: worker.GroupClaimVersion, HostClaimVersion: worker.HostClaimVersion}
	var claimed run.ExecutionAuthority
	var resumeWait *db.RunWait
	err := s.inTx(ctx, func(work *txWork) error {
		tx := work.tx
		var restored bool
		if err := tx.QueryRow(ctx, `SELECT status='running' FROM run_leases WHERE id=$1`, leaseID).Scan(&restored); err != nil {
			return staleRunLeaseClaim(err)
		}
		var err error
		if restored {
			var wait db.RunWait
			claimed, wait, err = run.ClaimRestoredExecution(ctx, tx, fence)
			resumeWait = &wait
		} else {
			claimed, err = run.ClaimExecution(ctx, tx, fence)
		}
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		return nil
	})
	if err != nil {
		return runLeaseClaimAuthority{}, nil, err
	}
	return runLeaseClaimAuthority{resumeWait: resumeWait, actor: claimed.Session, run: claimed.Run, computer: claimed.Computer, attempt: claimed.Attempt, runtime: claimed.Instance, runLease: claimed.Lease}, claimed.Secrets, nil
}

func staleRunLeaseClaim(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errStaleRunLeaseClaim
	}
	return err
}
