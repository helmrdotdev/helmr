package run

import (
	"bytes"
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

// Execution is a Run lease execution locked by a run fence in the owning
// transaction: the Run, its current Attempt, its Session when it is an Actor
// Run, its Computer and Instance, and its lease. Only run fences construct it;
// it is valid only inside the transaction that locked it. Accessors return
// copies.
type Execution struct {
	run      db.Run
	attempt  db.RunAttempt
	session  db.Session
	instance computer.RunInstance
	lease    db.RunLease
	secrets  []secret.DeliveryEnvelope
}

// Run is the locked Run.
func (e Execution) Run() db.Run { return cloneRun(e.run) }

// Attempt is the locked current Attempt.
func (e Execution) Attempt() db.RunAttempt { return cloneAttempt(e.attempt) }

// Session is the locked Session of an Actor Run; it is the zero value for a
// Task Run.
func (e Execution) Session() db.Session { return cloneSession(e.session) }

// Computer is the locked Computer of the Run.
func (e Execution) Computer() db.LockRunLeaseClaimComputerRow { return e.instance.Computer() }

// Instance is the locked Instance the lease runs on.
func (e Execution) Instance() db.ComputerInstance { return e.instance.Instance() }

// Lease is the locked Run lease.
func (e Execution) Lease() db.RunLease { return cloneLease(e.lease) }

// DeliverySecrets are the attempt's Secret deliveries locked by the fence, or
// nil when the fence locks none.
func (e Execution) DeliverySecrets() []secret.DeliveryEnvelope {
	if e.secrets == nil {
		return nil
	}
	secrets := make([]secret.DeliveryEnvelope, len(e.secrets))
	for n, envelope := range e.secrets {
		envelope.Version.Nonce = bytes.Clone(envelope.Version.Nonce)
		envelope.Version.Ciphertext = bytes.Clone(envelope.Version.Ciphertext)
		secrets[n] = envelope
	}
	return secrets
}

// The clone helpers copy every slice of a row, so a caller cannot change what
// an accessor returns next through a returned row.

func cloneRun(r db.Run) db.Run {
	r.Payload = bytes.Clone(r.Payload)
	r.Output = bytes.Clone(r.Output)
	r.Failure = bytes.Clone(r.Failure)
	r.Metadata = bytes.Clone(r.Metadata)
	r.Tags = slices.Clone(r.Tags)
	r.RetryPolicy = bytes.Clone(r.RetryPolicy)
	return r
}

func cloneAttempt(a db.RunAttempt) db.RunAttempt {
	a.TerminalError = bytes.Clone(a.TerminalError)
	return a
}

func cloneSession(s db.Session) db.Session {
	s.Failure = bytes.Clone(s.Failure)
	s.RunRetryPolicy = bytes.Clone(s.RunRetryPolicy)
	s.RunMetadata = bytes.Clone(s.RunMetadata)
	s.RunTags = slices.Clone(s.RunTags)
	return s
}

func cloneLease(l db.RunLease) db.RunLease {
	l.TerminalError = bytes.Clone(l.TerminalError)
	return l
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
func lockExecution(ctx context.Context, tx pgx.Tx, request ExecutionFence, operation executionOperation, target executionTarget) (Execution, error) {
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
		return Execution{}, errors.New("invalid execution operation")
	}
	if err != nil {
		return Execution{}, err
	}
	if operation != executionLive && operation != executionResume && loc.RunWaitID.Valid {
		return Execution{}, pgx.ErrNoRows
	}
	result := Execution{}
	if operation == executionClaim {
		result.secrets, err = secret.LockAttemptDelivery(ctx, q, loc.RunID, loc.AttemptNumber, loc.ComputerID)
		if err != nil {
			return Execution{}, err
		}
	}
	if err := workergroup.LockExecutionHost(ctx, q, request.host(loc.RegionID, operation == executionClaim || operation == executionStart)); err != nil {
		return Execution{}, err
	}
	lineage, err := cancellationLineage(ctx, tx, pgvalue.MustUUIDValue(loc.RunID))
	if err != nil {
		return Execution{}, err
	}
	if err = lockExecutionComputers(ctx, tx, lineage, loc.EnvironmentID, target); err != nil {
		return Execution{}, err
	}
	access := computer.RunAdmission
	switch operation {
	case executionLive:
		access = computer.RunLive
	case executionResume:
		access = computer.RunResume
	}
	result.instance, err = computer.LockInstanceForRun(ctx, tx, computer.RunInstanceRef{
		OrgID: pgvalue.MustUUIDValue(loc.OrgID), ProjectID: pgvalue.MustUUIDValue(loc.ProjectID), EnvironmentID: pgvalue.MustUUIDValue(loc.EnvironmentID), RegionID: loc.RegionID,
		ComputerID: pgvalue.MustUUIDValue(loc.ComputerID), InstanceID: pgvalue.MustUUIDValue(loc.ComputerInstanceID),
		Host:             computer.Host{GroupID: pgvalue.MustUUIDValue(request.WorkerGroupID), HostID: pgvalue.MustUUIDValue(request.WorkerHostID), Epoch: request.WorkerEpoch},
		WriterGeneration: loc.WriterGeneration,
	}, access)
	if err != nil {
		return Execution{}, err
	}
	c, i := result.instance.Computer(), result.instance.Instance()
	scope := CancellationRequest{OrgID: pgvalue.MustUUIDValue(loc.OrgID), ProjectID: pgvalue.MustUUIDValue(loc.ProjectID), EnvironmentID: pgvalue.MustUUIDValue(loc.EnvironmentID)}
	if err = lockExecutionSessions(ctx, tx, scope, lineage, target.session); err != nil {
		return Execution{}, err
	}
	order := slices.Clone(lineage)
	slices.SortFunc(order, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	for _, id := range order {
		r, e := q.LockRunFinalizationParentRun(ctx, db.LockRunFinalizationParentRunParams{ID: pgvalue.UUID(id), OrgID: loc.OrgID, ProjectID: loc.ProjectID, EnvironmentID: loc.EnvironmentID})
		if e != nil {
			return Execution{}, e
		}
		if r.ID == loc.RunID {
			result.run = r
		} else if r.Status != db.RunStatusQueued && r.Status != db.RunStatusRunning && r.Status != db.RunStatusWaiting && r.Status != db.RunStatusRetryDelayed {
			return Execution{}, pgx.ErrNoRows
		}
	}
	current, err := cancellationLineage(ctx, tx, pgvalue.MustUUIDValue(loc.RunID))
	if err != nil {
		return Execution{}, err
	}
	if !slices.Equal(current, lineage) {
		return Execution{}, pgx.ErrNoRows
	}
	r := result.run
	if (operation == executionClaim && r.Status != db.RunStatusQueued) || (operation == executionStart && r.Status != db.RunStatusQueued && r.Status != db.RunStatusRunning) || ((operation == executionLive || operation == executionResume) && r.Status != db.RunStatusRunning && r.Status != db.RunStatusWaiting) {
		return Execution{}, pgx.ErrNoRows
	}
	if r.ComputerID != c.ID || r.CurrentAttemptNumber != loc.AttemptNumber || r.CurrentRunLeaseID != request.LeaseID || r.SessionID != loc.SessionID || r.DeploymentID != i.ProgramDeploymentID {
		return Execution{}, pgx.ErrNoRows
	}
	if r.SessionID.Valid {
		result.session, err = q.LockRunLeaseClaimActor(ctx, db.LockRunLeaseClaimActorParams{ID: r.SessionID, ComputerID: c.ID})
		if err != nil {
			return Execution{}, err
		}
		s := result.session
		if r.EntrypointKind != "actor" || s.CurrentRunID != r.ID || !loc.ActorRunGeneration.Valid || s.RunGeneration != loc.ActorRunGeneration.Int64 || s.CancelRequestedAt.Valid || (operation != executionLive && r.Status == db.RunStatusQueued && s.ActiveTurnID.Valid) || (s.Status != "open" && s.Status != "closing") || s.DeploymentDefinitionID != r.DeploymentDefinitionID || s.ActorDeclaredID != r.EntrypointDeclaredID {
			return Execution{}, pgx.ErrNoRows
		}
		// A stop hold blocks admission, but the exact running generation must
		// remain able to acknowledge work already admitted before the stop.
		if s.DispatchHoldID.Valid && ((operation != executionLive && operation != executionResume) || s.DispatchHoldReason.String != "interrupt_requested" || s.DispatchHoldRunID != r.ID || !s.DispatchHoldAttemptNumber.Valid || s.DispatchHoldAttemptNumber.Int32 != r.CurrentAttemptNumber || !s.DispatchHoldRunGeneration.Valid || s.DispatchHoldRunGeneration.Int64 != s.RunGeneration) {
			return Execution{}, pgx.ErrNoRows
		}
	} else if r.EntrypointKind != "task" {
		return Execution{}, pgx.ErrNoRows
	}
	result.attempt, err = q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{RunID: r.ID, Number: r.CurrentAttemptNumber, ComputerID: c.ID})
	if err != nil {
		return Execution{}, err
	}
	a := result.attempt
	if a.TerminalAt.Valid || a.EntrypointKind != r.EntrypointKind {
		return Execution{}, pgx.ErrNoRows
	}
	if r.Status == db.RunStatusQueued && r.SessionID.Valid && (!a.SessionInputStartSequence.Valid || !r.SessionInputStartSequence.Valid || !r.SessionInputHighWatermark.Valid || result.session.CommittedInputSequence != a.SessionInputStartSequence.Int64 || a.SessionInputStartSequence.Int64 < r.SessionInputStartSequence.Int64 || r.SessionInputHighWatermark.Int64 < a.SessionInputStartSequence.Int64 || r.SessionInputHighWatermark.Int64 >= result.session.NextInputSequence) {
		return Execution{}, pgx.ErrNoRows
	}
	switch operation {
	case executionClaim:
		result.lease, err = q.LockRunLeaseClaimLease(ctx, db.LockRunLeaseClaimLeaseParams{ID: request.LeaseID, RunID: r.ID, ComputerID: c.ID, AttemptNumber: a.Number, LeaseSequence: request.LeaseSequence})
	case executionStart:
		result.lease, err = q.LockRunStartLease(ctx, db.LockRunStartLeaseParams{ID: request.LeaseID, RunID: r.ID, ComputerID: c.ID, AttemptNumber: a.Number, LeaseSequence: request.LeaseSequence})
	case executionLive, executionResume:
		result.lease, err = q.LockLiveRunLease(ctx, db.LockLiveRunLeaseParams{ID: request.LeaseID, RunID: r.ID, ComputerID: c.ID, AttemptNumber: a.Number, LeaseSequence: request.LeaseSequence})
	}

	if err != nil {
		return Execution{}, err
	}
	l := result.lease
	if l.ComputerInstanceID != i.ID || l.WriterGeneration != i.WriterGeneration || l.WorkerGroupID != request.WorkerGroupID || l.WorkerHostID != request.WorkerHostID || l.WorkerEpoch != request.WorkerEpoch || l.DeploymentID != r.DeploymentID {
		return Execution{}, pgx.ErrNoRows
	}
	// Evaluate time only after every potentially blocking authority lock.
	live, err := q.GetRunLeaseExecutionLive(ctx, db.GetRunLeaseExecutionLiveParams{ID: l.ID, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds, SkipWorkerReadiness: operation == executionLive || operation == executionResume})
	if err != nil {
		return Execution{}, err
	}
	if !live {
		return Execution{}, pgx.ErrNoRows
	}
	return result, nil
}

// ClaimExecution grants no VM ownership and never restores parked members.
func ClaimExecution(ctx context.Context, tx pgx.Tx, request ExecutionFence) (Execution, error) {
	result, err := lockExecution(ctx, tx, request, executionClaim, executionTarget{})
	if err != nil {
		return Execution{}, err
	}
	l := result.lease
	if l.Status == db.RunLeaseStatusAssigned {
		result.lease, err = db.New(tx).MarkRunLeaseStarting(ctx, db.MarkRunLeaseStartingParams{ID: l.ID, LeaseSequence: l.LeaseSequence, WorkerGroupID: l.WorkerGroupID, WorkerHostID: l.WorkerHostID, WorkerEpoch: l.WorkerEpoch})
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
