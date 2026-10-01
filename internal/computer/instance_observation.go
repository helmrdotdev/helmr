package computer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Observation is a worker host's physical observation of one Instance
// incarnation, fenced by the observed version it acted on. Observations
// carry no claim versions: the locked worker epoch and status are their worker
// authority.
type Observation struct {
	Instance                InstanceRef
	ExpectedObservedVersion int64
}

// Readiness observes the Instance ready with the VM shape the host assigned.
type Readiness struct {
	Observation
	VCPUCount       int32
	CPUConfigDigest string
}

// Closure observes the Instance closed with physical exclusion evidence.
type Closure struct {
	Observation
	Reason       string
	CleanupProof *CleanupProof
}

// Cleanup proof methods: how the host established that no process of the
// Instance incarnation can still run.
const (
	// CleanupMachineClosed reports that the Instance machine closed.
	CleanupMachineClosed = "machine_closed"
	// CleanupHostReconciled reports exact reconciliation of the host's VMs.
	CleanupHostReconciled = "host_reconciled"
	// CleanupNotMaterialized reports that the Instance never started a VM.
	CleanupNotMaterialized = "not_materialized"
)

// CleanupProof is the physical exclusion evidence an Instance records when it
// is reclaimed. Its JSON encoding is the stored reclaim evidence.
type CleanupProof struct {
	Method      string    `json:"method"`
	CompletedAt time.Time `json:"completed_at"`
}

// evidence validates the proof at now and encodes it. A closed Instance
// requires a closed machine or exact host reconciliation; a failed Instance
// may also never have materialized.
func (p CleanupProof) evidence(now time.Time, closed bool) ([]byte, error) {
	if closed && p.Method != CleanupMachineClosed && p.Method != CleanupHostReconciled {
		return nil, invalidInput("closed instance cleanup proof must confirm a closed machine or exact host reconciliation")
	}
	switch p.Method {
	case CleanupMachineClosed, CleanupHostReconciled, CleanupNotMaterialized:
	default:
		return nil, invalidInput("instance cleanup proof method is unsupported")
	}
	if p.CompletedAt.IsZero() || p.CompletedAt.After(now.Add(time.Minute)) {
		return nil, invalidInput("instance cleanup proof completed_at is required and cannot be in the future")
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return nil, invalidInput("encode instance cleanup proof")
	}
	return encoded, nil
}

// FailureKind classifies a reported Instance failure by the authority it
// affects beyond the Instance.
type FailureKind uint8

const (
	// FailureInstance is a failure of the Instance alone.
	FailureInstance FailureKind = iota
	// FailureWorkerInvalid reports that the worker epoch itself is invalid; the
	// host is drained.
	FailureWorkerInvalid
	// FailureSourceUnavailable reports that the Computer's committed source
	// cannot be prepared; the Computer requires recovery.
	FailureSourceUnavailable
)

// Failure observes the Instance failed. With CleanupProof it also reclaims
// the failed Instance.
type Failure struct {
	Observation
	Kind         FailureKind
	Reason       string
	Error        []byte
	CleanupProof *CleanupProof
}

// RecordInstanceReady acknowledges the assigned VM shape and current
// observation in one transaction under supply → Computer → Instance locks.
// Fresh preparation succeeds at this commit; a frozen restore retains its
// preparation budget until the activation that opens admission.
func RecordInstanceReady(ctx context.Context, txb db.TxBeginner, readiness Readiness) (db.ComputerInstance, error) {
	var row db.ComputerInstance
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var err error
		row, err = recordReady(ctx, tx, readiness)
		return err
	})
	return row, authorityChanged(err)
}

func recordReady(ctx context.Context, tx pgx.Tx, readiness Readiness) (db.ComputerInstance, error) {
	observed, err := lockReadyObservation(ctx, tx, readiness.Instance)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	ref := readiness.Instance
	row, err := db.New(tx).MarkComputerInstanceReady(ctx, db.MarkComputerInstanceReadyParams{
		DesiredVersion: ref.DesiredVersion, ID: pgvalue.UUID(ref.ID), WorkerHostID: pgvalue.UUID(ref.Host.HostID),
		WorkerEpoch: ref.Host.Epoch, WriterGeneration: observed.instance.WriterGeneration,
		ExpectedObservedVersion: readiness.ExpectedObservedVersion,
		VMVCPUCount:             readiness.VCPUCount, CPUConfigDigest: readiness.CPUConfigDigest,
	})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if !row.SourceCheckpointID.Valid {
		if err = completePendingPreparation(ctx, tx, row); err != nil {
			return db.ComputerInstance{}, err
		}
	}
	return row, nil
}

// RecordInstanceClosed validates the physical exclusion evidence and accepts
// it in one transaction under Computer → Instance locks. A replay with the
// original fence returns the durable receipt.
func RecordInstanceClosed(ctx context.Context, txb db.TxBeginner, closure Closure) (db.ComputerInstance, error) {
	if closure.CleanupProof == nil {
		return db.ComputerInstance{}, invalidInput("instance cleanup proof is required when marking an instance closed")
	}
	evidence, err := closure.CleanupProof.evidence(time.Now(), true)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	row, err := reclaimInstance(ctx, txb, closure.Instance.Host.GroupID, reclaimParams(closure.Observation, closure.Reason, evidence, false))
	return row, authorityChanged(err)
}

func reclaimParams(o Observation, reason string, evidence []byte, requireFailure bool) db.ReclaimComputerInstanceParams {
	ref := o.Instance
	return db.ReclaimComputerInstanceParams{RequireFailure: requireFailure,
		ID: pgvalue.UUID(ref.ID), WorkerHostID: pgvalue.UUID(ref.Host.HostID), WorkerEpoch: ref.Host.Epoch,
		DesiredVersion: ref.DesiredVersion, ExpectedObservedVersion: o.ExpectedObservedVersion,
		Reason: pgvalue.Text(reason), Evidence: evidence,
	}
}

// RecordInstanceFailure records a reported Instance failure as a sequence of
// transactions:
//
//  1. A FailureWorkerInvalid report first drains the worker epoch in its own
//     transaction; a failed drain fails the report. The drain precedes
//     validation of the cleanup proof.
//  2. With CleanupProof, it validates the proof; an Instance already failed
//     at the reported fences is reclaimed and the report ends there.
//  3. It records the failure under supply → Computer → Instance locks. When
//     that fence no longer holds for a FailureWorkerInvalid report, it drains
//     the worker epoch again in its own transaction.
//  4. With CleanupProof, it reclaims the Instance it just failed.
//
// Logical failure settlement of the Instance's members remains with their
// owners.
func RecordInstanceFailure(ctx context.Context, txb db.TxBeginner, failure Failure) (db.ComputerInstance, error) {
	host := failure.Instance.Host
	workerInvalid := failure.Kind == FailureWorkerInvalid
	if workerInvalid {
		if err := workergroup.DrainInvalidEpoch(ctx, txb, host.GroupID, host.HostID, host.Epoch); err != nil {
			// The drain precedes any Instance fence, so its failure never
			// reports changed Instance authority.
			return db.ComputerInstance{}, fmt.Errorf("drain invalid worker epoch: %v", err)
		}
	}
	var evidence []byte
	if failure.CleanupProof != nil {
		var err error
		if evidence, err = failure.CleanupProof.evidence(time.Now(), false); err != nil {
			return db.ComputerInstance{}, err
		}
		row, err := reclaimInstance(ctx, txb, host.GroupID, reclaimParams(failure.Observation, failure.Reason, evidence, true))
		if err == nil || !errors.Is(err, pgx.ErrNoRows) {
			return row, err
		}
	}
	var row db.ComputerInstance
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var err error
		row, err = recordFailure(ctx, tx, failure)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) && workerInvalid {
		if drainErr := workergroup.DrainInvalidEpoch(ctx, txb, host.GroupID, host.HostID, host.Epoch); drainErr != nil {
			return row, authorityChanged(drainErr)
		}
	}
	if err == nil && failure.CleanupProof != nil {
		row, err = reclaimInstance(ctx, txb, host.GroupID, db.ReclaimComputerInstanceParams{RequireFailure: true,
			ID: row.ID, WorkerHostID: row.WorkerHostID, WorkerEpoch: row.WorkerEpoch, DesiredVersion: row.DesiredVersion,
			ExpectedObservedVersion: row.ObservedVersion, Reason: row.TerminalReasonCode, Evidence: evidence,
		})
	}
	return row, authorityChanged(err)
}

// recordFailure records a physical failure under supply → Computer → Instance
// locks. A FailureWorkerInvalid report drains an active host in the same
// transaction.
func recordFailure(ctx context.Context, tx pgx.Tx, failure Failure) (db.ComputerInstance, error) {
	q := db.New(tx)
	ref := failure.Instance
	id, workerID := pgvalue.UUID(ref.ID), pgvalue.UUID(ref.Host.HostID)
	target, err := q.GetWorkerComputerInstanceTarget(ctx, db.GetWorkerComputerInstanceTargetParams{ID: id, WorkerHostID: workerID, WorkerEpoch: ref.Host.Epoch, WorkerGroupID: pgvalue.UUID(ref.Host.GroupID)})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	locked, err := workergroup.LockHostWithPool(ctx, q, ref.Host.GroupID, pgvalue.MustUUIDValue(target.WorkerPoolID), ref.Host.HostID, ref.Host.Epoch)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: target.EnvironmentID, ID: target.ComputerID})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	i, err := q.LockWorkerComputerInstance(ctx, db.LockWorkerComputerInstanceParams{ID: id, OrgID: target.OrgID, WorkerHostID: workerID, WorkerGroupID: locked.Group.ID, WorkerEpoch: ref.Host.Epoch})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if i.ComputerID != c.ID || i.EnvironmentID != c.EnvironmentID || i.WriterGeneration != c.WriterGeneration {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	if failure.Kind == FailureSourceUnavailable {
		if i.ObservedState != "allocated" {
			return db.ComputerInstance{}, pgx.ErrNoRows
		}
		// Only the retained committed head can condemn published Computer data.
		// A private restore failure cannot relabel the independent committed head.
		_, err = tx.Exec(ctx, `UPDATE computers c SET status='recovery_required',desired_state='stopped',dirty_state='dirty_state_lost',
   recovery_id=CASE WHEN recovery_completed_at IS NULL THEN coalesce(recovery_id,$2) ELSE $2 END,
   recovery_disk_version_id=head_disk_version_id,recovery_reason='computer_source_unavailable',
   recovery_started_at=CASE WHEN recovery_completed_at IS NULL THEN coalesce(recovery_started_at,clock_timestamp()) ELSE clock_timestamp() END,
   recovery_completed_at=NULL,recovery_failure=coalesce(recovery_failure,jsonb_build_object('code','computer_source_unavailable','message','Published Computer source is unavailable','details',$3::jsonb)),
   revision=revision+1,updated_at=clock_timestamp()
   FROM computer_instances i,computer_disk_versions v WHERE i.id=$1 AND c.id=i.computer_id
   AND c.status NOT IN ('deleting','deleted') AND c.desired_state<>'deleted'
   AND c.head_disk_version_id=i.retained_source_disk_version_id AND v.id=c.head_disk_version_id AND v.computer_id=c.id AND v.status='committed'`, i.ID, pgvalue.UUID(uuid.NewV7()), failure.Error)
		if err != nil {
			return db.ComputerInstance{}, err
		}
	}
	row, err := q.MarkComputerInstanceFailed(ctx, db.MarkComputerInstanceFailedParams{
		ReasonCode: pgvalue.Text(failure.Reason), Error: failure.Error,
		ID: id, WorkerHostID: workerID, WorkerEpoch: ref.Host.Epoch,
		DesiredVersion:          ref.DesiredVersion,
		ExpectedObservedVersion: failure.ExpectedObservedVersion,
	})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if failure.Kind == FailureWorkerInvalid {
		if err = workergroup.DrainLockedHost(ctx, q, locked); err != nil {
			return db.ComputerInstance{}, err
		}
	}
	return row, nil
}

// reclaimInstance accepts an authenticated physical exclusion receipt in one
// transaction under Computer → Instance locks.
func reclaimInstance(ctx context.Context, txb db.TxBeginner, groupID uuid.UUID, params db.ReclaimComputerInstanceParams) (db.ComputerInstance, error) {
	var row db.ComputerInstance
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var err error
		row, err = lockedReclaim(ctx, tx, groupID, params)
		return err
	})
	return row, err
}

func lockedReclaim(ctx context.Context, tx pgx.Tx, groupID uuid.UUID, params db.ReclaimComputerInstanceParams) (db.ComputerInstance, error) {
	q := db.New(tx)
	target, err := q.GetWorkerComputerInstanceTarget(ctx, db.GetWorkerComputerInstanceTargetParams{ID: params.ID, WorkerHostID: params.WorkerHostID, WorkerEpoch: params.WorkerEpoch, WorkerGroupID: pgvalue.UUID(groupID)})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if _, err = q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: target.EnvironmentID, ID: target.ComputerID}); err != nil {
		return db.ComputerInstance{}, err
	}
	i, err := q.LockWorkerComputerInstance(ctx, db.LockWorkerComputerInstanceParams{ID: params.ID, OrgID: target.OrgID, WorkerHostID: params.WorkerHostID, WorkerEpoch: params.WorkerEpoch, WorkerGroupID: pgvalue.UUID(groupID)})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if i.ReclaimedAt.Valid {
		// A lost cleanup acknowledgment may be replayed with its original fence.
		// Return the durable receipt without changing capacity or failure diagnosis.
		if i.DesiredState == "closed" && i.DesiredVersion == params.DesiredVersion &&
			i.ObservedVersion > 0 && i.ObservedVersion-1 == params.ExpectedObservedVersion &&
			(!params.RequireFailure || i.ObservedState == "failed" || i.ObservedState == "lost") {
			return i, nil
		}
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	params.WriterGeneration = i.WriterGeneration
	// Keep the original failure diagnosis even after successful physical cleanup.
	if i.ObservedState == "failed" || i.ObservedState == "lost" {
		params.ObservedState = i.ObservedState
		params.Reason = i.TerminalReasonCode
		params.Error = i.TerminalError
	} else {
		params.ObservedState = "closed"
		params.Error = nil
	}
	params.MountState = "unmounted"
	reclaimed, err := q.ReclaimComputerInstance(ctx, params)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	// Physical exclusion settles every process in this exact incarnation,
	// including members whose scoped exit proof was lost. Logical outcomes are
	// still owned by the Run and checkpoint reconciliation paths.
	if _, err := tx.Exec(ctx, `UPDATE run_leases SET process_reconciled_at=$3,updated_at=clock_timestamp()
 WHERE computer_instance_id=$1 AND writer_generation=$2 AND process_reconciled_at IS NULL`,
		reclaimed.ID, reclaimed.WriterGeneration, reclaimed.ReclaimedAt); err != nil {
		return db.ComputerInstance{}, err
	}
	return reclaimed, nil
}
