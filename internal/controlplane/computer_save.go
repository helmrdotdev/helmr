package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type computerSaveOperation int

const (
	computerSaveBegin computerSaveOperation = iota
	computerSaveWrite
	computerSaveAdopt
	computerSaveAbandon
)

// beginComputerSave binds the pending operation under the existing execution lock
// order. Admission does not publish bytes, change the execution origin or release
// retention. Checkpoint/finalization coordination is owned by the host publisher.
func (s *Server) beginComputerSave(ctx context.Context, worker workerActor, request workerapi.ComputerSaveBeginRequest) (workerapi.ComputerSaveBeginResponse, error) {
	return s.withComputerSave(ctx, worker, request, computerSaveBegin, nil)
}

func (s *Server) withComputerSave(ctx context.Context, worker workerActor, request workerapi.ComputerSaveBeginRequest, operation computerSaveOperation, apply func(pgx.Tx, *db.Queries, db.RuntimeInstance, db.WorkspaceLease) error) (workerapi.ComputerSaveBeginResponse, error) {
	var result workerapi.ComputerSaveBeginResponse
	save, err := parseCanonicalUUID("save_id", request.SaveID)
	if err != nil {
		return result, badRequest(err)
	}
	if request.Sequence <= 0 || (request.Lease == nil && (request.OrgID == "" || request.WorkspaceMountID == "")) || (request.Lease != nil && (request.OrgID != "" || request.WorkspaceMountID != "")) {
		return result, badRequest(errors.New("one execution authority and positive save sequence required"))
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := db.New(tx)
	var runtime db.RuntimeInstance
	var lease db.WorkspaceLease
	var head pgtype.UUID
	var expires pgtype.Timestamptz
	var hardDeadline time.Time
	var computerReady bool
	var executing bool
	var activeBudget bool
	if request.Lease != nil {
		parsed, err := parseRunLeaseFence(*request.Lease)
		if err != nil {
			return result, badRequest(err)
		}
		locators, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: request.Lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch})
		if err != nil {
			return result, err
		}
		if _, err = secret.LockAttemptDelivery(ctx, q, locators.RunID, locators.AttemptNumber, locators.WorkspaceID); err != nil {
			return result, err
		}
		a, err := lockRunPublicationAuthority(ctx, q, worker, pgvalue.UUID(parsed.leaseID), request.Lease.LeaseSequence, locators, db.RunStatusRunning, db.RunStatusWaiting)
		if err != nil {
			return result, err
		}
		if err = validateRunFinalizationOwner(a, locators); err != nil {
			return result, err
		}
		executing = a.runLease.Status == db.RunLeaseStatusRunning && a.run.Status == db.RunStatusRunning
		settling := a.run.Status == db.RunStatusWaiting && (a.runLease.Status == db.RunLeaseStatusRunning || a.runLease.Status == db.RunLeaseStatusCheckpointing)
		if !executing && !settling {
			return result, computerObjectConflict("execution cannot settle saves")
		}
		activeBudget = a.run.ActiveStartedAt.Valid && a.run.MaxActiveDurationMs > 0 && a.run.ActiveElapsedMs >= 0 && a.run.ActiveElapsedMs < a.run.MaxActiveDurationMs
		if activeBudget {
			hardDeadline = a.run.ActiveStartedAt.Time.Add(time.Duration(a.run.MaxActiveDurationMs-a.run.ActiveElapsedMs) * time.Millisecond)
		}

		runtime, lease, head, expires = a.runtime, a.workspaceLease, a.workspace.HeadVersionID, a.runLease.ExpiresAt
		computerReady = a.workspace.Status == "active" && a.workspace.DesiredState == "active"
	} else {
		org, mount, err := parseWorkspaceWorkerIDs(request.OrgID, request.WorkspaceMountID)
		if err != nil {
			return result, badRequest(err)
		}
		locator, err := q.GetWorkspaceExecLocatorForMount(ctx, db.GetWorkspaceExecLocatorForMountParams{OrgID: org, WorkspaceMountID: mount})
		if err != nil {
			return result, err
		}
		valid, err := lockWorkspaceExecPublicationSecrets(ctx, q, locator.ID, locator.WorkspaceID)
		if err != nil {
			return result, err
		}
		if !valid {
			return result, computerObjectConflict("exec secrets revoked")
		}
		c, err := q.LockWorkspaceExecFailureWorkspace(ctx, db.LockWorkspaceExecFailureWorkspaceParams{OrgID: org, WorkspaceID: locator.WorkspaceID})
		if err != nil {
			return result, err
		}
		a, err := q.LockWorkspaceExecWorkerAuthority(ctx, db.LockWorkspaceExecWorkerAuthorityParams{OrgID: org, ProcessID: locator.ID, WorkspaceMountID: mount, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch, ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds})
		if err != nil {
			return result, err
		}
		executing = a.WorkspaceProcess.Status == db.WorkspaceProcessStatusRunning && a.WorkspaceMount.Status == db.WorkspaceMountStatusMounted
		if !executing {
			return result, computerObjectConflict("execution is not running")
		}
		runtime, lease, head = a.RuntimeInstance, a.WorkspaceLease, a.SavedHeadVersionID
		computerReady = c.Status == "active" && c.DesiredState == "active"
	}
	if !computerReady || runtime.DesiredState != "ready" || runtime.ObservedState != "ready" || runtime.ObservedDesiredVersion != runtime.DesiredVersion || runtime.ReclaimedAt.Valid || lease.Status != db.WorkspaceLeaseStatusActive {
		return result, computerObjectConflict("save execution authority is no longer active")
	}
	var claims bool
	if err = tx.QueryRow(ctx, `SELECT w.claim_version=$3 AND g.claim_version=$4 FROM worker_instances w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1 AND g.id=$2`, pgvalue.UUID(worker.WorkerInstanceID), pgvalue.UUID(worker.WorkerGroupID), worker.ClaimVersion, worker.GroupClaimVersion).Scan(&claims); err != nil {
		return result, err
	}
	if !claims {
		return result, computerObjectConflict("worker claims changed")
	}
	pending := runtime.ComputerSaveID == pgvalue.UUID(save) && runtime.ComputerSaveSequence == request.Sequence && runtime.ComputerSaveLeaseID == lease.ID
	settlement := operation == computerSaveAdopt || operation == computerSaveAbandon || (operation == computerSaveBegin && pending)
	if !settlement {
		if !executing {
			return result, computerObjectConflict("new save work requires running execution")
		}
		if request.Lease != nil && !activeBudget {
			return result, computerObjectConflict("run execution budget exhausted")
		}
	} else {
		// This permits only an exact pending-operation receipt/cleanup, never
		// fresh bytes or a new admission after execution enters managed waiting.
		hardDeadline = time.Time{}
	}
	predecessor := runtime.ComputerSavePredecessorID
	if operation == computerSaveBegin && !pending {
		row, err := q.BeginRuntimeComputerSave(ctx, db.BeginRuntimeComputerSaveParams{Sequence: request.Sequence, SaveID: pgvalue.UUID(save), LeaseID: lease.ID, PredecessorID: head, RuntimeInstanceID: runtime.ID, WorkerInstanceID: runtime.WorkerInstanceID, WorkerEpoch: runtime.WorkerEpoch, DesiredVersion: runtime.DesiredVersion})
		if err != nil {
			return result, err
		}
		predecessor = row.ComputerSavePredecessorID
	} else if !pending {
		return result, computerObjectConflict("save operation is not pending")
	}

	if operation != computerSaveBegin {
		var published bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_versions WHERE publisher_runtime_instance_id=$1 AND publisher_save_sequence=$2)`, runtime.ID, request.Sequence).Scan(&published); err != nil {
			return result, err
		}
		if published != (operation == computerSaveAdopt) {
			return result, computerObjectConflict("save publication state differs from operation")
		}
	}
	if apply != nil {
		if err := apply(tx, q, runtime, lease); err != nil {
			return result, err
		}
	}
	now, err := q.GetRunLeaseRenewalTime(ctx)
	if err != nil {
		return result, err
	}
	if !now.Valid || !lease.ExpiresAt.Valid || !now.Time.Before(lease.ExpiresAt.Time) || (expires.Valid && !now.Time.Before(expires.Time)) || (!hardDeadline.IsZero() && !now.Time.Before(hardDeadline)) {
		return result, computerObjectConflict("save deadline expired")
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return workerapi.ComputerSaveBeginResponse{RuntimeInstanceID: pgvalue.UUIDString(runtime.ID), WorkspaceLeaseID: pgvalue.UUIDString(lease.ID), PredecessorID: pgvalue.UUIDString(predecessor), DesiredVersion: runtime.DesiredVersion, SaveID: request.SaveID, Sequence: request.Sequence}, nil
}
