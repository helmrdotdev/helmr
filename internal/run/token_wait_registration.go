package run

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// TokenWaitFence is the Run lease a worker host registers a Token wait for,
// on the worker epoch it authenticated.
type TokenWaitFence struct {
	RunLeaseID    uuid.UUID
	LeaseSequence int64
	WorkerGroupID uuid.UUID
	WorkerHostID  uuid.UUID
	WorkerEpoch   int64
}

// Token wait registration locks its authority in three stages so that the
// token owner can replay an existing registration and check the Session
// cursor between them, in this order:
//
//  1. BeginTokenWaitRegistration reads the live lease and the Run's
//     locators, locks worker_groups and worker_hosts without comparing claim
//     versions, the Computer and its Instance, the Session of an Actor Run
//     and the Run;
//  2. TokenWaitStage.LockAttempt checks the Run against the lease, then
//     locks the current Attempt;
//  3. TokenWaitAttempt.LockLease locks the running Run lease.
//
// Every stage returns a descriptive error for a rejected or failed step; the
// token owner reports each one as a Token wait authority failure. The
// stages are valid only inside the transaction that began them.

// tokenWaitScope is the locked registration authority each stage exposes.
type tokenWaitScope struct {
	tx       pgx.Tx
	fence    TokenWaitFence
	lease    db.GetLiveRunLeaseLocatorsRow
	owner    db.GetTokenWaitRegistrationLocatorRow
	instance db.ComputerInstance
	session  db.Session
	run      db.Run
}

// Lease is the live lease locator the registration read before any lock.
func (s tokenWaitScope) Lease() db.GetLiveRunLeaseLocatorsRow { return s.lease }

// Owner is the Run's Computer, Session and scope, read before any lock.
func (s tokenWaitScope) Owner() db.GetTokenWaitRegistrationLocatorRow { return s.owner }

// Session is the locked Session of an Actor Run; it is the zero value for a
// Task Run.
func (s tokenWaitScope) Session() db.Session { return cloneSession(s.session) }

// Run is the locked Run.
func (s tokenWaitScope) Run() db.Run { return cloneRun(s.run) }

// TokenWaitStage is a Token wait registration whose worker host, Computer,
// Instance, Session and Run are locked.
type TokenWaitStage struct{ tokenWaitScope }

// BeginTokenWaitRegistration locks the first stage of a Token wait
// registration.
//
// Equivalence: it issues the live lease locator read, the registration
// locator read, workergroup.LockExecutionHostWithoutClaims,
// computer.LockTokenWaitInstance, the Session lock of an Actor Run (which
// must be open or closing) and the Run lock, in that order, with the
// predicates those steps document and no claim version comparison, Secret,
// lineage or execution-live check.
func BeginTokenWaitRegistration(ctx context.Context, tx pgx.Tx, fence TokenWaitFence) (TokenWaitStage, error) {
	q := db.New(tx)
	lease, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
		ID: pgvalue.UUID(fence.RunLeaseID), LeaseSequence: fence.LeaseSequence,
		WorkerGroupID: pgvalue.UUID(fence.WorkerGroupID), WorkerHostID: pgvalue.UUID(fence.WorkerHostID),
		WorkerEpoch: fence.WorkerEpoch,
	})
	if err != nil {
		return TokenWaitStage{}, tokenWaitError("load token wait lease authority", err)
	}
	owner, err := q.GetTokenWaitRegistrationLocator(ctx, db.GetTokenWaitRegistrationLocatorParams{
		EnvironmentID: lease.EnvironmentID, RunID: lease.RunID,
	})
	if err != nil {
		return TokenWaitStage{}, tokenWaitError("load token wait registration locator", err)
	}
	platform, err := workergroup.LockExecutionHostWithoutClaims(ctx, q, workergroup.ExecutionEpoch{
		GroupID: pgvalue.UUID(fence.WorkerGroupID), RegionID: lease.RegionID,
		HostID: pgvalue.UUID(fence.WorkerHostID), Epoch: fence.WorkerEpoch,
	})
	if err != nil {
		return TokenWaitStage{}, tokenWaitHostError(err)
	}
	instance, err := computer.LockTokenWaitInstance(ctx, tx, computer.TokenWaitInstanceRef{
		OrgID: pgvalue.MustUUIDValue(owner.OrgID), ProjectID: pgvalue.MustUUIDValue(owner.ProjectID),
		EnvironmentID: pgvalue.MustUUIDValue(lease.EnvironmentID), RegionID: lease.RegionID,
		ComputerID: pgvalue.MustUUIDValue(owner.ComputerID), InstanceID: pgvalue.MustUUIDValue(lease.ComputerInstanceID),
		Host:         computer.Host{GroupID: fence.WorkerGroupID, HostID: fence.WorkerHostID, Epoch: fence.WorkerEpoch},
		VMPlatformID: platform, WriterGeneration: lease.WriterGeneration,
	})
	if err != nil {
		return TokenWaitStage{}, tokenWaitInstanceError(err)
	}
	var session db.Session
	if owner.SessionID.Valid {
		session, err = q.LockTokenWaitSession(ctx, owner.SessionID)
		if err != nil {
			return TokenWaitStage{}, tokenWaitError("lock owning actor", err)
		}
		if session.Status != "open" && session.Status != "closing" {
			return TokenWaitStage{}, tokenWaitError("owning actor is not active", nil)
		}
	}
	locked, err := q.LockTokenWaitRun(ctx, db.LockTokenWaitRunParams{EnvironmentID: lease.EnvironmentID, RunID: lease.RunID})
	if err != nil {
		return TokenWaitStage{}, tokenWaitError("lock Run", err)
	}
	return TokenWaitStage{tokenWaitScope{tx: tx, fence: fence, lease: lease, owner: owner, instance: instance, session: session, run: locked}}, nil
}

// TokenWaitAttempt is a Token wait registration whose current Attempt is
// also locked.
type TokenWaitAttempt struct {
	tokenWaitScope
	attempt db.LockTokenWaitAttemptRow
}

// LockAttempt requires the locked Run to be running on the Run's Computer
// at the lease's Attempt, current lease and active start, then locks the
// current Attempt, which must not be terminal.
//
// Equivalence: the Run checks read only the locked Run; the Attempt
// statement is the Run, number and Computer scoped Attempt lock FOR UPDATE.
func (s TokenWaitStage) LockAttempt(ctx context.Context) (TokenWaitAttempt, error) {
	if s.tx == nil {
		return TokenWaitAttempt{}, errors.New("token wait registration is not locked")
	}
	r := s.run
	if r.ComputerID != s.owner.ComputerID || db.RunStatus(r.Status) != db.RunStatusRunning ||
		r.CurrentAttemptNumber != s.lease.AttemptNumber ||
		!r.CurrentRunLeaseID.Valid || uuid.UUID(r.CurrentRunLeaseID.Bytes) != s.fence.RunLeaseID ||
		!r.ActiveStartedAt.Valid {
		return TokenWaitAttempt{}, tokenWaitError("run registration fence does not match", nil)
	}
	attempt, err := db.New(s.tx).LockTokenWaitAttempt(ctx, db.LockTokenWaitAttemptParams{
		RunID: s.lease.RunID, AttemptNumber: s.lease.AttemptNumber, ComputerID: s.owner.ComputerID,
	})
	if err != nil || attempt.TerminalAt.Valid {
		return TokenWaitAttempt{}, tokenWaitError("lock current run attempt", err)
	}
	return TokenWaitAttempt{tokenWaitScope: s.tokenWaitScope, attempt: attempt}, nil
}

// Attempt is the locked current Attempt.
func (a TokenWaitAttempt) Attempt() db.LockTokenWaitAttemptRow { return a.attempt }

// LockLease locks the Run lease, which must be the running, unexpired lease
// of the fence on the locked Instance's VM platform. The token owner
// registers the Wait only after it succeeds.
//
// Equivalence: this is the Token wait Run lease statement FOR UPDATE with
// the fence's lease, sequence, worker group, host and epoch, the lease's
// Run, Attempt, Computer, Instance and region, and the locked Instance's VM
// platform.
func (a TokenWaitAttempt) LockLease(ctx context.Context) error {
	if a.tx == nil {
		return errors.New("token wait registration is not locked")
	}
	status, err := db.New(a.tx).LockTokenWaitRunLease(ctx, db.LockTokenWaitRunLeaseParams{
		ID:                 pgvalue.UUID(a.fence.RunLeaseID),
		RunID:              a.lease.RunID,
		AttemptNumber:      a.lease.AttemptNumber,
		ComputerID:         a.owner.ComputerID,
		LeaseSequence:      a.fence.LeaseSequence,
		WorkerGroupID:      pgvalue.UUID(a.fence.WorkerGroupID),
		WorkerHostID:       pgvalue.UUID(a.fence.WorkerHostID),
		WorkerEpoch:        a.fence.WorkerEpoch,
		ComputerInstanceID: a.lease.ComputerInstanceID,
		VMPlatformID:       a.instance.VMPlatformID,
		RegionID:           a.lease.RegionID,
	})
	if err != nil || db.RunLeaseStatus(status) != db.RunLeaseStatusRunning {
		return tokenWaitError("lock current unexpired run lease", err)
	}
	return nil
}

func tokenWaitError(operation string, cause error) error {
	if cause == nil {
		return errors.New(operation)
	}
	return fmt.Errorf("%s: %w", operation, cause)
}

// tokenWaitHostError names the worker supply lock that rejected the
// registration, with the statement's error when one failed.
func tokenWaitHostError(err error) error {
	operation, cause := "lock worker group", err
	var rejected *workergroup.ExecutionHostError
	if errors.As(err, &rejected) {
		cause = rejected.Err
		if rejected.Host {
			operation = "lock current worker epoch"
		}
	}
	return tokenWaitError(operation, cause)
}

// tokenWaitInstanceError names the Computer or Instance lock that rejected
// the registration, with the statement's error when one failed.
func tokenWaitInstanceError(err error) error {
	operation, cause := "lock active Computer", err
	var rejected *computer.TokenWaitInstanceError
	if errors.As(err, &rejected) {
		cause = rejected.Err
		if rejected.Instance {
			operation = "lock ready runtime"
		}
	}
	return tokenWaitError(operation, cause)
}
