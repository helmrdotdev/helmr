package run

import (
	"context"
	"errors"
	"slices"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

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
	if err := workergroup.LockExecutionHost(ctx, q, request.host(loc.RegionID, operation == executionClaim || operation == executionStart)); err != nil {
		return ExecutionAuthority{}, err
	}
	lineage, err := cancellationLineage(ctx, tx, pgvalue.MustUUIDValue(loc.RunID))
	if err != nil {
		return ExecutionAuthority{}, err
	}
	if err = lockExecutionComputers(ctx, tx, lineage, loc.EnvironmentID, target); err != nil {
		return ExecutionAuthority{}, err
	}
	access := computer.RunAdmission
	switch operation {
	case executionLive:
		access = computer.RunLive
	case executionResume:
		access = computer.RunResume
	}
	result.Computer, result.Instance, err = computer.LockInstanceForRun(ctx, tx, computer.RunInstanceRef{
		OrgID: pgvalue.MustUUIDValue(loc.OrgID), ProjectID: pgvalue.MustUUIDValue(loc.ProjectID), EnvironmentID: pgvalue.MustUUIDValue(loc.EnvironmentID), RegionID: loc.RegionID,
		ComputerID: pgvalue.MustUUIDValue(loc.ComputerID), InstanceID: pgvalue.MustUUIDValue(loc.ComputerInstanceID),
		Host:             computer.Host{GroupID: pgvalue.MustUUIDValue(request.WorkerGroupID), HostID: pgvalue.MustUUIDValue(request.WorkerHostID), Epoch: request.WorkerEpoch},
		WriterGeneration: loc.WriterGeneration,
	}, access)
	if err != nil {
		return ExecutionAuthority{}, err
	}
	c, i := result.Computer, result.Instance
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
	live, err := q.GetRunLeaseExecutionLive(ctx, db.GetRunLeaseExecutionLiveParams{ID: l.ID, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds, SkipWorkerReadiness: operation == executionLive || operation == executionResume})
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

// host fences the lease's Worker Group and Host under the worker supply
// lifecycle rule documented in workergroup/supply.go: claim and start
// (admission) accept an active or draining Group, so already dispatched leases
// finish, but reject a paused Group; operations continuing an already started
// Run also accept a paused Group.
func (f ExecutionFence) host(region string, admission bool) workergroup.ExecutionHost {
	return workergroup.ExecutionHost{
		GroupID: f.WorkerGroupID, RegionID: region, HostID: f.WorkerHostID, Epoch: f.WorkerEpoch,
		GroupClaimVersion: f.GroupClaimVersion, HostClaimVersion: f.HostClaimVersion, Admission: admission,
	}
}
