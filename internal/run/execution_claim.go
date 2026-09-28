package run

import (
	"context"
	"errors"
	"slices"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrExecutionWorkerClaims = errors.New("execution Worker claims changed")

type ExecutionFence struct {
	LeaseID, WorkerGroupID, WorkerHostID                            pgtype.UUID
	LeaseSequence, WorkerEpoch, GroupClaimVersion, HostClaimVersion int64
}

type ExecutionAuthority struct {
	Run      db.Run
	Attempt  db.RunAttempt
	Session  db.Session
	Computer db.LockRunLeaseClaimComputerRow
	Instance db.ComputerInstance
	Lease    db.RunLease
	Secrets  []secret.DeliveryEnvelope
}

type executionOperation uint8

const (
	executionClaim executionOperation = iota
	executionStart
	executionLive
	executionResume
)

// lockExecution locks supply, Computers/Instances, then Session/Run lineage.
// Its caller owns commit/rollback and the operation-specific transition.
func lockExecution(ctx context.Context, tx pgx.Tx, request ExecutionFence, operation executionOperation, target executionTarget) (ExecutionAuthority, error) {
	q := db.New(tx)
	var loc db.GetRunLeaseClaimLocatorsRow
	var err error
	switch operation {
	case executionClaim:
		loc, err = q.GetRunLeaseClaimLocators(ctx, db.GetRunLeaseClaimLocatorsParams{ID: request.LeaseID, LeaseSequence: request.LeaseSequence, WorkerGroupID: request.WorkerGroupID, WorkerHostID: request.WorkerHostID, WorkerEpoch: request.WorkerEpoch})
	case executionStart:
		var row db.GetRunLeaseStartLocatorsRow
		row, err = q.GetRunLeaseStartLocators(ctx, db.GetRunLeaseStartLocatorsParams{ID: request.LeaseID, LeaseSequence: request.LeaseSequence, WorkerGroupID: request.WorkerGroupID, WorkerHostID: request.WorkerHostID, WorkerEpoch: request.WorkerEpoch})
		loc = db.GetRunLeaseClaimLocatorsRow(row)
	case executionLive, executionResume:
		var row db.GetLiveRunLeaseLocatorsRow
		row, err = q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{ID: request.LeaseID, LeaseSequence: request.LeaseSequence, WorkerGroupID: request.WorkerGroupID, WorkerHostID: request.WorkerHostID, WorkerEpoch: request.WorkerEpoch})
		loc = db.GetRunLeaseClaimLocatorsRow(row)
	default:
		return ExecutionAuthority{}, errors.New("invalid execution operation")
	}
	if err != nil {
		return ExecutionAuthority{}, err
	}
	if operation != executionLive && operation != executionResume && loc.RunWaitID.Valid {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	result := ExecutionAuthority{}
	if operation == executionClaim {
		result.Secrets, err = secret.LockAttemptDelivery(ctx, q, loc.RunID, loc.AttemptNumber, loc.ComputerID)
		if err != nil {
			return ExecutionAuthority{}, err
		}
	}
	if err := lockExecutionWorker(ctx, q, request, loc.RegionID); err != nil {
		return ExecutionAuthority{}, err
	}
	lineage, err := cancellationLineage(ctx, tx, pgvalue.MustUUIDValue(loc.RunID))
	if err != nil {
		return ExecutionAuthority{}, err
	}
	if err = lockExecutionPlacement(ctx, tx, lineage, loc.EnvironmentID, target); err != nil {
		return ExecutionAuthority{}, err
	}
	result.Computer, err = q.LockRunLeaseClaimComputer(ctx, db.LockRunLeaseClaimComputerParams{ID: loc.ComputerID, OrgID: loc.OrgID, ProjectID: loc.ProjectID, EnvironmentID: loc.EnvironmentID, RegionID: loc.RegionID})
	if err != nil {
		return ExecutionAuthority{}, err
	}
	result.Instance, err = q.LockRunLeaseClaimInstance(ctx, db.LockRunLeaseClaimInstanceParams{ID: loc.ComputerInstanceID, OrgID: loc.OrgID, ProjectID: loc.ProjectID, EnvironmentID: loc.EnvironmentID, RegionID: loc.RegionID, WorkerGroupID: request.WorkerGroupID, WorkerHostID: request.WorkerHostID, WorkerEpoch: request.WorkerEpoch, ComputerID: loc.ComputerID})
	if err != nil {
		return ExecutionAuthority{}, err
	}
	c, i := result.Computer, result.Instance
	if c.Status != "active" || c.DesiredState != "active" || c.DeletedAt.Valid || c.DirtyState == "dirty_state_lost" || c.DirtyState == "capture_failed" || i.WriterGeneration != c.WriterGeneration || i.WriterGeneration != loc.WriterGeneration || (i.AdmissionState != "open" && !(operation == executionLive && (i.AdmissionState == "draining" || i.AdmissionState == "checkpointing")) && !(operation == executionResume && (i.AdmissionState == "restoring" || i.AdmissionState == "draining"))) || i.DesiredState != "ready" || i.ObservedState != "ready" || i.ObservedDesiredVersion != i.DesiredVersion || i.MountState != "mounted" || i.ReclaimedAt.Valid || i.TerminalAt.Valid {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	scope := CancellationRequest{OrgID: pgvalue.MustUUIDValue(loc.OrgID), ProjectID: pgvalue.MustUUIDValue(loc.ProjectID), EnvironmentID: pgvalue.MustUUIDValue(loc.EnvironmentID)}
	if err = lockExecutionSessions(ctx, tx, scope, lineage, target.session); err != nil {
		return ExecutionAuthority{}, err
	}
	order := slices.Clone(lineage)
	slices.SortFunc(order, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	for _, id := range order {
		r, e := q.LockRunFinalizationParentRun(ctx, db.LockRunFinalizationParentRunParams{ID: pgvalue.UUID(id), OrgID: loc.OrgID, ProjectID: loc.ProjectID, EnvironmentID: loc.EnvironmentID})
		if e != nil {
			return ExecutionAuthority{}, e
		}
		if r.ID == loc.RunID {
			result.Run = r
		} else if r.Status != db.RunStatusQueued && r.Status != db.RunStatusRunning && r.Status != db.RunStatusWaiting && r.Status != db.RunStatusRetryDelayed {
			return ExecutionAuthority{}, pgx.ErrNoRows
		}
	}
	current, err := cancellationLineage(ctx, tx, pgvalue.MustUUIDValue(loc.RunID))
	if err != nil {
		return ExecutionAuthority{}, err
	}
	if !slices.Equal(current, lineage) {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	r := result.Run
	if (operation == executionClaim && r.Status != db.RunStatusQueued) || (operation == executionStart && r.Status != db.RunStatusQueued && r.Status != db.RunStatusRunning) || ((operation == executionLive || operation == executionResume) && r.Status != db.RunStatusRunning && r.Status != db.RunStatusWaiting) {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	if r.ComputerID != c.ID || r.CurrentAttemptNumber != loc.AttemptNumber || r.CurrentRunLeaseID != request.LeaseID || r.SessionID != loc.SessionID || r.DeploymentID != i.ProgramDeploymentID {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	if r.SessionID.Valid {
		result.Session, err = q.LockRunLeaseClaimActor(ctx, db.LockRunLeaseClaimActorParams{ID: r.SessionID, ComputerID: c.ID})
		if err != nil {
			return ExecutionAuthority{}, err
		}
		s := result.Session
		if r.EntrypointKind != "actor" || s.CurrentRunID != r.ID || !loc.ActorRunGeneration.Valid || s.RunGeneration != loc.ActorRunGeneration.Int64 || s.CancelRequestedAt.Valid || (operation != executionLive && r.Status == db.RunStatusQueued && s.ActiveTurnID.Valid) || (s.Status != "open" && s.Status != "closing") || s.DeploymentDefinitionID != r.DeploymentDefinitionID || s.ActorDeclaredID != r.EntrypointDeclaredID {
			return ExecutionAuthority{}, pgx.ErrNoRows
		}
		// A stop hold blocks admission, but the exact running generation must
		// remain able to acknowledge work already admitted before the stop.
		if s.DispatchHoldID.Valid && ((operation != executionLive && operation != executionResume) || s.DispatchHoldReason.String != "interrupt_requested" || s.DispatchHoldRunID != r.ID || !s.DispatchHoldAttemptNumber.Valid || s.DispatchHoldAttemptNumber.Int32 != r.CurrentAttemptNumber || !s.DispatchHoldRunGeneration.Valid || s.DispatchHoldRunGeneration.Int64 != s.RunGeneration) {
			return ExecutionAuthority{}, pgx.ErrNoRows
		}
	} else if r.EntrypointKind != "task" {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	result.Attempt, err = q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{RunID: r.ID, Number: r.CurrentAttemptNumber, ComputerID: c.ID})
	if err != nil {
		return ExecutionAuthority{}, err
	}
	a := result.Attempt
	if a.TerminalAt.Valid || a.EntrypointKind != r.EntrypointKind {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	if r.Status == db.RunStatusQueued && r.SessionID.Valid && (!a.SessionInputStartSequence.Valid || !r.SessionInputStartSequence.Valid || !r.SessionInputHighWatermark.Valid || result.Session.CommittedInputSequence != a.SessionInputStartSequence.Int64 || a.SessionInputStartSequence.Int64 < r.SessionInputStartSequence.Int64 || r.SessionInputHighWatermark.Int64 < a.SessionInputStartSequence.Int64 || r.SessionInputHighWatermark.Int64 >= result.Session.NextInputSequence) {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	switch operation {
	case executionClaim:
		result.Lease, err = q.LockRunLeaseClaimLease(ctx, db.LockRunLeaseClaimLeaseParams{ID: request.LeaseID, RunID: r.ID, ComputerID: c.ID, AttemptNumber: a.Number, LeaseSequence: request.LeaseSequence})
	case executionStart:
		result.Lease, err = q.LockRunStartLease(ctx, db.LockRunStartLeaseParams{ID: request.LeaseID, RunID: r.ID, ComputerID: c.ID, AttemptNumber: a.Number, LeaseSequence: request.LeaseSequence})
	case executionLive, executionResume:
		result.Lease, err = q.LockLiveRunLease(ctx, db.LockLiveRunLeaseParams{ID: request.LeaseID, RunID: r.ID, ComputerID: c.ID, AttemptNumber: a.Number, LeaseSequence: request.LeaseSequence})
	}

	if err != nil {
		return ExecutionAuthority{}, err
	}
	l := result.Lease
	if l.ComputerInstanceID != i.ID || l.WriterGeneration != i.WriterGeneration || l.WorkerGroupID != request.WorkerGroupID || l.WorkerHostID != request.WorkerHostID || l.WorkerEpoch != request.WorkerEpoch || l.DeploymentID != r.DeploymentID {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	// Evaluate time only after every potentially blocking authority lock.
	var live bool
	err = tx.QueryRow(ctx, `SELECT l.expires_at>clock_timestamp() AND (l.status NOT IN ('assigned','starting') OR l.start_deadline_at>clock_timestamp())
 AND i.writer_expires_at>clock_timestamp() AND ($3 OR (h.observed_at>=clock_timestamp()-$2*interval '1 second'
 AND h.run_paused_reason IS NULL)) FROM run_leases l JOIN computer_instances i ON i.id=l.computer_instance_id
 JOIN worker_hosts h ON h.id=l.worker_host_id WHERE l.id=$1`, l.ID, workerapi.WorkerObservationFreshnessSeconds, (operation == executionLive || operation == executionResume)).Scan(&live)
	if err != nil {
		return ExecutionAuthority{}, err
	}
	if !live {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	return result, nil
}

// ClaimExecution grants no VM ownership and never restores parked members.
func ClaimExecution(ctx context.Context, tx pgx.Tx, request ExecutionFence) (ExecutionAuthority, error) {
	result, err := lockExecution(ctx, tx, request, executionClaim, executionTarget{})
	if err != nil {
		return ExecutionAuthority{}, err
	}
	l := result.Lease
	if l.Status == db.RunLeaseStatusAssigned {
		result.Lease, err = db.New(tx).MarkRunLeaseStarting(ctx, db.MarkRunLeaseStartingParams{ID: l.ID, LeaseSequence: l.LeaseSequence, WorkerGroupID: l.WorkerGroupID, WorkerHostID: l.WorkerHostID, WorkerEpoch: l.WorkerEpoch})
	}
	return result, err
}

func lockExecutionWorker(ctx context.Context, q *db.Queries, request ExecutionFence, region string) error {
	group, err := q.LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{ID: request.WorkerGroupID, RegionID: region})
	if err != nil {
		return err
	}
	if group.ClaimVersion != request.GroupClaimVersion {
		return ErrExecutionWorkerClaims
	}
	if group.Status != db.WorkerGroupStatusActive && group.Status != db.WorkerGroupStatusDraining {
		return pgx.ErrNoRows
	}
	host, err := q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{ID: request.WorkerHostID, WorkerGroupID: request.WorkerGroupID})
	if err != nil {
		return err
	}
	if host.ClaimVersion != request.HostClaimVersion {
		return ErrExecutionWorkerClaims
	}
	if !host.CurrentEpoch.Valid || host.CurrentEpoch.Int64 != request.WorkerEpoch || (host.Status != db.WorkerHostStatusActive && host.Status != db.WorkerHostStatusDraining) {
		return pgx.ErrNoRows
	}
	return nil
}
