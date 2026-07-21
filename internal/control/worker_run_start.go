package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerStart(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerRunStartRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid worker Run start request JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid worker Run start request JSON: trailing value")))
		return
	}
	leaseID, err := uuid.Parse(strings.TrimSpace(request.Lease.ID))
	if err != nil || request.Lease.LeaseSequence <= 0 {
		writeError(w, badRequest(errors.New("lease.id must be a UUID and lease.lease_sequence must be positive")))
		return
	}
	receipt, err := s.startFreshRun(
		r.Context(),
		workerFromContext(r.Context()),
		pgvalue.UUID(leaseID),
		request.Lease,
	)
	if err != nil {
		if errors.Is(err, errStaleRunLeaseClaim) {
			writeError(w, conflict(errors.New("Run start acknowledgement is stale")))
			return
		}
		s.log.Error("start fresh Run failed", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("start fresh Run"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerRunStartResponse{Lease: receipt})
}

func (s *Server) startFreshRun(
	ctx context.Context,
	worker workerActor,
	leaseID pgtype.UUID,
	expected api.WorkerRunLeaseReceipt,
) (api.WorkerRunLeaseReceipt, error) {
	var receipt api.WorkerRunLeaseReceipt
	err := s.inTx(ctx, func(work *txWork) error {
		locators, err := work.q.GetFreshRunLeaseStartLocators(ctx, db.GetFreshRunLeaseStartLocatorsParams{
			ID:                    leaseID,
			LeaseSequence:         expected.LeaseSequence,
			WorkerGroupID:         worker.WorkerGroupID,
			WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:           worker.WorkerEpoch,
			WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		authority, err := lockFreshRunStartAuthority(ctx, work.q, worker, leaseID, expected.LeaseSequence, locators)
		if err != nil {
			return err
		}
		receipt, err = projectRunLeaseReceipt(runLeaseProjectionAuthority{
			run:            authority.run,
			attempt:        authority.attempt,
			runtime:        authority.runtime,
			networkSlot:    authority.networkSlot,
			runLease:       authority.runLease,
			workspace:      authority.workspace,
			workspaceMount: authority.workspaceMount,
			workspaceLease: authority.workspaceLease,
		})
		if err != nil {
			return err
		}
		if !equalRunLeaseReceipt(receipt, expected) {
			return errStaleRunLeaseClaim
		}
		switch authority.runLease.State {
		case db.RunLeaseStateStarting:
			if authority.run.Status != db.RunStatusQueued ||
				authority.attempt.EntrypointEnteredAt.Valid {
				return errStaleRunLeaseClaim
			}
			authority.runLease, err = work.q.MarkFreshRunLeaseRunning(ctx, db.MarkFreshRunLeaseRunningParams{
				ID:                    authority.runLease.ID,
				RunID:                 authority.run.ID,
				WorkspaceID:           authority.workspace.ID,
				AttemptNumber:         authority.attempt.Number,
				LeaseSequence:         authority.runLease.LeaseSequence,
				WorkerGroupID:         worker.WorkerGroupID,
				WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
				WorkerEpoch:           worker.WorkerEpoch,
				WorkerProtocolVersion: worker.ProtocolVersion,
				RuntimeInstanceID:     authority.runtime.ID,
				NetworkSlotID:         authority.networkSlot.ID,
				NetworkSlotGeneration: authority.networkSlot.Generation,
				RuntimeIdentityID:     authority.runtime.RuntimeIdentityID,
			})
			if err != nil {
				return staleRunLeaseClaim(err)
			}
			authority.run, err = work.q.MarkFreshRunRunning(ctx, db.MarkFreshRunRunningParams{
				ID:                   authority.run.ID,
				OrgID:                authority.run.OrgID,
				ProjectID:            authority.run.ProjectID,
				EnvironmentID:        authority.run.EnvironmentID,
				WorkspaceID:          authority.workspace.ID,
				ExpectedStateVersion: authority.run.StateVersion,
				AttemptNumber:        authority.attempt.Number,
				RunLeaseID:           authority.runLease.ID,
			})
			if err != nil {
				return staleRunLeaseClaim(err)
			}
			authority.workspace, err = work.q.TouchFreshRunWorkspace(ctx, db.TouchFreshRunWorkspaceParams{
				ID:                  authority.workspace.ID,
				OrgID:               authority.workspace.OrgID,
				ProjectID:           authority.workspace.ProjectID,
				EnvironmentID:       authority.workspace.EnvironmentID,
				OwnershipGeneration: authority.workspaceLease.OwnershipGeneration,
				WriterGeneration:    authority.workspaceLease.WriterGeneration,
				RunID:               authority.run.ID,
			})
			if err != nil {
				return staleRunLeaseClaim(err)
			}
		case db.RunLeaseStateRunning:
			if authority.run.Status != db.RunStatusRunning {
				return errStaleRunLeaseClaim
			}
		default:
			return errStaleRunLeaseClaim
		}
		return nil
	})
	if err != nil {
		return api.WorkerRunLeaseReceipt{}, err
	}
	return receipt, nil
}

func lockFreshRunStartAuthority(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetFreshRunLeaseStartLocatorsRow,
) (runLeaseClaimAuthority, error) {
	authority := runLeaseClaimAuthority{mode: runLeaseClaimFresh}
	var err error
	authority.run, err = q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
		ID:            locators.RunID,
		OrgID:         locators.OrgID,
		ProjectID:     locators.ProjectID,
		EnvironmentID: locators.EnvironmentID,
		WorkspaceID:   locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.run.EntrypointKind != "task" ||
		authority.run.ActorID.Valid ||
		authority.run.ParentRunID.Valid ||
		authority.run.CurrentAttemptNumber != locators.AttemptNumber ||
		authority.run.CurrentRunLeaseID != leaseID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.workspace, err = q.LockRunLeaseClaimWorkspace(ctx, db.LockRunLeaseClaimWorkspaceParams{
		ID:            locators.WorkspaceID,
		OrgID:         locators.OrgID,
		ProjectID:     locators.ProjectID,
		EnvironmentID: locators.EnvironmentID,
		RegionID:      locators.RegionID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateClaimWorkspace(authority.run, authority.workspace); err != nil {
		return runLeaseClaimAuthority{}, err
	}

	authority.attempt, err = q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
		RunID:       locators.RunID,
		Number:      locators.AttemptNumber,
		WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.attempt.TerminalAt.Valid ||
		authority.attempt.EntrypointKind != authority.run.EntrypointKind ||
		authority.attempt.BaseWorkspaceVersionID != authority.run.BaseWorkspaceVersionID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.workerGroup, err = q.LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{
		ID:       worker.WorkerGroupID,
		RegionID: locators.RegionID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.workerGroup.State != db.WorkerGroupStateActive ||
		!authority.workerGroup.AllowsRun ||
		authority.workerGroup.ClaimVersion != worker.GroupClaimVersion ||
		authority.workerGroup.ProtocolVersion != worker.ProtocolVersion {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.worker, err = q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{
		ID:            pgvalue.UUID(worker.WorkerInstanceID),
		WorkerGroupID: worker.WorkerGroupID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateClaimWorker(worker, authority.worker); err != nil {
		return runLeaseClaimAuthority{}, err
	}

	authority.networkSlot, err = q.LockRunLeaseClaimNetworkSlot(ctx, db.LockRunLeaseClaimNetworkSlotParams{
		ID:                locators.NetworkSlotID,
		WorkerGroupID:     worker.WorkerGroupID,
		WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:       worker.WorkerEpoch,
		Generation:        locators.NetworkSlotGeneration,
		RuntimeInstanceID: locators.RuntimeInstanceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.networkSlot.State != db.WorkerNetworkSlotStateBound {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.runtime, err = q.LockRunLeaseClaimRuntime(ctx, db.LockRunLeaseClaimRuntimeParams{
		ID:               locators.RuntimeInstanceID,
		OrgID:            locators.OrgID,
		ProjectID:        locators.ProjectID,
		EnvironmentID:    locators.EnvironmentID,
		RegionID:         locators.RegionID,
		WorkerGroupID:    worker.WorkerGroupID,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:      worker.WorkerEpoch,
		WorkspaceID:      locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}

	authority.runLease, err = q.LockFreshRunStartLease(ctx, db.LockFreshRunStartLeaseParams{
		ID:            leaseID,
		RunID:         locators.RunID,
		WorkspaceID:   locators.WorkspaceID,
		AttemptNumber: locators.AttemptNumber,
		LeaseSequence: leaseSequence,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateClaimPhysicalAuthority(worker, authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}

	authority.workspaceMount, err = q.LockRunLeaseClaimMount(ctx, db.LockRunLeaseClaimMountParams{
		ID:                locators.WorkspaceMountID,
		OrgID:             locators.OrgID,
		ProjectID:         locators.ProjectID,
		EnvironmentID:     locators.EnvironmentID,
		RegionID:          locators.RegionID,
		WorkerGroupID:     worker.WorkerGroupID,
		WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:       worker.WorkerEpoch,
		RuntimeInstanceID: locators.RuntimeInstanceID,
		WorkspaceID:       locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}

	authority.workspaceLease, err = q.LockRunLeaseClaimWorkspaceLease(ctx, db.LockRunLeaseClaimWorkspaceLeaseParams{
		ID:                locators.WorkspaceLeaseID,
		OrgID:             locators.OrgID,
		ProjectID:         locators.ProjectID,
		EnvironmentID:     locators.EnvironmentID,
		RegionID:          locators.RegionID,
		WorkerGroupID:     worker.WorkerGroupID,
		WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:       worker.WorkerEpoch,
		RuntimeInstanceID: locators.RuntimeInstanceID,
		WorkspaceID:       locators.WorkspaceID,
		WorkspaceMountID:  locators.WorkspaceMountID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateClaimWorkspaceAuthority(
		authority,
		authority.attempt.BaseWorkspaceVersionID,
	); err != nil {
		return runLeaseClaimAuthority{}, err
	}
	return authority, nil
}
