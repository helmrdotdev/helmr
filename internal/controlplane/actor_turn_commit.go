package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleActorTurnCommit = errors.New("actor turn commit is stale")

type parsedActorTurnCommit struct {
	turnID                 uuid.UUID
	generation             int64
	disposition            string
	result                 json.RawMessage
	fingerprint            string
	lease                  parsedRunLeaseFence
	correlationID          uuid.UUID
	targetInputSequence    int64
	baseWorkspaceVersionID uuid.UUID
	tree                   workspace.TreeIdentity
	artifact               *workerapi.WorkspaceArtifact
}

func parseActorTurnCommitRequest(request workerapi.CommitActorTurnRequest) (parsedActorTurnCommit, error) {
	turnID, err := parseCanonicalUUID("turn_id", request.TurnID)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	if request.RunGeneration <= 0 {
		return parsedActorTurnCommit{}, errors.New("run_generation must be positive")
	}
	if request.Disposition != "completed" && request.Disposition != "failed" {
		return parsedActorTurnCommit{}, errors.New("disposition must be completed or failed")
	}
	payload := request.Result
	if request.Disposition == "failed" {
		if len(request.Result) != 0 || len(request.Error) == 0 {
			return parsedActorTurnCommit{}, errors.New("failed settlement requires error and forbids result")
		}
		payload = request.Error
	} else if len(request.Error) != 0 {
		return parsedActorTurnCommit{}, errors.New("completed settlement forbids error")
	}
	var result json.RawMessage
	if len(payload) != 0 {
		result, err = canonicalJSON(payload)
		if err != nil {
			return parsedActorTurnCommit{}, errors.New("settlement payload must be valid JSON")
		}
	}
	if request.Disposition == "failed" {
		request.Error = result
	} else {
		request.Result = result
	}
	fingerprint, err := terminalRequestFingerprint("worker.turn.settle.v1", request)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	correlationID, err := parseCanonicalUUID("correlation_id", request.CorrelationID)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	baseWorkspaceVersionID, err := parseCanonicalUUID("base_workspace_version_id", request.BaseWorkspaceVersionID)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	if request.TargetInputSequence <= 0 {
		return parsedActorTurnCommit{}, errors.New("target_input_sequence must be positive")
	}
	tree, err := parseTaskWorkspaceTree("tree", request.Tree)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	if request.Artifact != nil {
		if err := validateTaskWorkspaceArtifact("artifact", *request.Artifact); err != nil {
			return parsedActorTurnCommit{}, err
		}
		if request.Artifact.EntryCount != request.Tree.EntryCount {
			return parsedActorTurnCommit{}, errors.New("artifact and tree entry counts differ")
		}
	}
	return parsedActorTurnCommit{
		turnID: turnID, generation: request.RunGeneration, disposition: request.Disposition, result: result, fingerprint: fingerprint,
		lease: lease, correlationID: correlationID, targetInputSequence: request.TargetInputSequence,
		baseWorkspaceVersionID: baseWorkspaceVersionID, tree: tree, artifact: request.Artifact,
	}, nil
}

func (s *Server) commitActorTurn(
	ctx context.Context,
	worker workerActor,
	request workerapi.CommitActorTurnRequest,
	commit parsedActorTurnCommit,
) (workerapi.CommitActorTurnResponse, error) {
	if commit.artifact != nil {
		capture := parsedWorkspaceTreeCapture{tree: commit.tree, artifact: *commit.artifact}
		if _, err := s.verifyWorkspaceTreeCapture(ctx, capture); err != nil {
			return workerapi.CommitActorTurnResponse{}, err
		}
	}

	var response workerapi.CommitActorTurnResponse
	err := s.inTx(ctx, func(work *txWork) error {
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(commit.lease.leaseID), LeaseSequence: request.Lease.LeaseSequence,
			WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch})
		if err != nil {
			return staleActorTurnCommit(err)
		}
		if _, err := secret.LockAttemptDelivery(
			ctx, work.q, locators.RunID, locators.AttemptNumber,
			locators.WorkspaceID,
		); err != nil {
			return fmt.Errorf("lock actor turn secret authority: %w", err)
		}
		owner, err := lockRunFinalizationOwner(ctx, work.q, locators)
		if err != nil || !owner.actor.ID.Valid {
			return staleActorTurnCommit(err)
		}
		authority, err := lockLiveRunLeaseAuthority(
			ctx, work.q, worker, pgvalue.UUID(commit.lease.leaseID), request.Lease.LeaseSequence, locators,
		)
		if err != nil {
			return staleActorTurnCommit(err)
		}
		authority.actor = owner.actor
		if err := validateActorTurnAuthority(ctx, work.q, authority); err != nil {
			return err
		}

		if authority.actor.CommittedInputSequence == commit.targetInputSequence {
			var replayed bool
			response, replayed, err = replayActorTurnCommit(ctx, work.q, request, commit, authority)
			if err != nil {
				return err
			}
			if replayed {
				return nil
			}
			return errStaleActorTurnCommit
		}
		scope := session.TurnScope{EnvironmentID: pgvalue.MustUUIDValue(authority.run.EnvironmentID), SessionID: pgvalue.MustUUIDValue(authority.actor.ID), TurnID: commit.turnID, RunID: pgvalue.MustUUIDValue(authority.run.ID), AttemptNumber: authority.attempt.Number, RunGeneration: commit.generation}
		input, err := session.ValidateTurn(ctx, work.q, scope)
		if err != nil {
			return staleActorTurnCommit(err)
		}
		if input.Sequence != commit.targetInputSequence {
			return errStaleActorTurnCommit
		}
		if authority.actor.CommittedInputSequence+1 != commit.targetInputSequence ||
			commit.targetInputSequence >= authority.actor.NextInputSequence ||
			authority.workspaceLease.BaseWorkspaceVersionID != pgvalue.UUID(commit.baseWorkspaceVersionID) {
			return errStaleActorTurnCommit
		}
		base, err := getActorTurnVersion(ctx, work.q, authority, authority.workspaceLease.BaseWorkspaceVersionID)
		if err != nil {
			return staleActorTurnCommit(err)
		}
		restoredBase := authority.workspaceLease.BaseWorkspaceVersionID != authority.workspace.HeadVersionID
		var restoredCheckpoint db.RunCheckpoint
		if restoredBase {
			restoredCheckpoint, err = validateRestoredActorBase(ctx, work.q, authority, base)
			if err != nil {
				return staleActorTurnCommit(err)
			}
		}
		changed := commit.tree.Digest != base.ContentDigest.String ||
			commit.tree.SizeBytes != base.LogicalSizeBytes || commit.tree.EntryCount != int(base.EntryCount)
		if changed != (commit.artifact != nil) {
			return errStaleActorTurnCommit
		}
		committedAt, err := work.q.GetTaskCompletionTime(ctx)
		if err != nil || !committedAt.Valid {
			if err == nil {
				err = errors.New("database actor turn commit time is unavailable")
			}
			return err
		}
		if !committedAt.Time.Before(authority.runLease.ExpiresAt.Time) ||
			!committedAt.Time.Before(authority.workspaceLease.ExpiresAt.Time) {
			return errStaleActorTurnCommit
		}
		if restoredBase {
			if _, err := work.q.InvalidateRestoredActorCheckpoint(
				ctx, db.InvalidateRestoredActorCheckpointParams{
					CommittedAt: committedAt, RestoreCheckpointID: authority.runtime.RestoreCheckpointID,
					RunID: authority.run.ID, AttemptNumber: authority.attempt.Number,
					WorkspaceID:               authority.workspace.ID,
					PrivateWorkspaceVersionID: restoredCheckpoint.PrivateWorkspaceVersionID,
					TargetInputSequence:       commit.targetInputSequence,
				},
			); err != nil {
				return staleActorTurnCommit(err)
			}
		}

		versionID := authority.workspaceLease.BaseWorkspaceVersionID
		if changed {
			versionID, err = recordTaskWorkspaceVersion(
				ctx, work.q, worker, authority,
				(parsedWorkspaceTreeCapture{tree: commit.tree, artifact: *commit.artifact}).version(), committedAt,
			)
			if err != nil {
				return err
			}
		} else if restoredBase {
			if _, err := work.q.PublishRestoredActorCheckpointWorkspaceVersion(
				ctx, db.PublishRestoredActorCheckpointWorkspaceVersionParams{
					CommittedAt: committedAt, VersionID: authority.workspaceLease.BaseWorkspaceVersionID,
					WorkspaceID: authority.workspace.ID, ExpectedParentVersionID: base.ParentVersionID,
					OwnershipGeneration: authority.workspace.OwnershipGeneration,
					WriterGeneration:    base.WriterGeneration,
					RestoreCheckpointID: authority.runtime.RestoreCheckpointID,
					RunID:               authority.run.ID, AttemptNumber: authority.attempt.Number,
				},
			); err != nil {
				return staleActorTurnCommit(err)
			}
		}
		if versionID != authority.workspace.HeadVersionID {
			previousHeadVersionID := authority.workspace.HeadVersionID
			updatedWorkspace, updateErr := work.q.AdvanceActorWorkspaceHead(ctx, db.AdvanceActorWorkspaceHeadParams{
				NewHeadVersionID: versionID, CompletedAt: committedAt, ID: authority.workspace.ID,
				OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
				EnvironmentID: authority.run.EnvironmentID, SessionID: authority.actor.ID,
				OwnershipGeneration:   authority.workspace.OwnershipGeneration,
				WriterGeneration:      authority.workspace.WriterGeneration,
				ExpectedHeadVersionID: previousHeadVersionID,
			})
			authority.workspace, err = db.LockRunLeaseClaimWorkspaceRow(updatedWorkspace), updateErr
			if err != nil {
				return staleActorTurnCommit(err)
			}
		}
		if changed {
			authority.workspaceMount.MaterializedVersionID = versionID
			authority.workspaceLease, err = work.q.AdvanceActorTurnWorkspaceLeaseFrontier(
				ctx, db.AdvanceActorTurnWorkspaceLeaseFrontierParams{
					NewVersionID: versionID, CommittedAt: committedAt, ID: authority.workspaceLease.ID,
					OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
					EnvironmentID: authority.run.EnvironmentID, WorkspaceID: authority.workspace.ID,
					WorkspaceMountID: authority.workspaceMount.ID, RuntimeInstanceID: authority.runtime.ID,
					OwnerRunLeaseID: authority.runLease.ID, ExpectedVersionID: pgvalue.UUID(commit.baseWorkspaceVersionID),
					OwnershipGeneration:    authority.workspace.OwnershipGeneration,
					WriterGeneration:       authority.workspace.WriterGeneration,
					MountFencingGeneration: authority.workspaceMount.FencingGeneration,
				},
			)
			if err != nil {
				return staleActorTurnCommit(err)
			}
		}
		event, err := session.SettleTurn(ctx, work.q, scope, commit.disposition, commit.result, versionID, commit.fingerprint)
		if err != nil {
			return staleActorTurnCommit(err)
		}
		response, err = projectActorTurnResponse(request, commit, versionID)
		response.EventID = pgvalue.UUIDString(event.ID)
		return err
	})
	return response, err
}

func validateActorTurnAuthority(ctx context.Context, store db.Querier, authority runLeaseClaimAuthority) error {
	actor := authority.actor
	if authority.run.EntrypointKind != "actor" || !authority.run.SessionID.Valid ||
		authority.run.SessionID != actor.ID || authority.run.ParentRunID.Valid ||
		authority.run.ParentOwnsLifecycle.Valid || authority.runLease.Status != db.RunLeaseStatusRunning ||
		!authority.run.ActiveStartedAt.Valid || !authority.attempt.EntrypointEnteredAt.Valid ||
		authority.attempt.TerminalAt.Valid || !authority.attempt.SessionInputStartSequence.Valid ||
		!authority.run.SessionInputStartSequence.Valid || !authority.run.SessionInputHighWatermark.Valid ||
		!actor.CurrentRunID.Valid || actor.CurrentRunID != authority.run.ID ||
		(actor.Status != "open" && actor.Status != "closing") ||
		authority.workspace.OwnerSessionID != actor.ID || authority.workspace.OwnerRunID.Valid ||
		!authority.workspace.HeadVersionID.Valid || authority.workspace.DirtyState != db.WorkspaceDirtyStateClean ||
		authority.runLease.FinalizationOperationID.Valid || authority.runLease.FinalizationKind.Valid ||
		authority.runLease.FinalizationStartedAt.Valid || authority.runLease.FinalizationRequestFingerprint.Valid {
		return errStaleActorTurnCommit
	}
	clear, err := store.RunFinalizationScopeIsClear(ctx, db.RunFinalizationScopeIsClearParams{
		RunID: authority.run.ID, AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
	})
	if err != nil {
		return err
	}
	if !clear.Valid || !clear.Bool {
		return errStaleActorTurnCommit
	}
	return nil
}

func replayActorTurnCommit(
	ctx context.Context,
	store db.Querier,
	request workerapi.CommitActorTurnRequest,
	commit parsedActorTurnCommit,
	authority runLeaseClaimAuthority,
) (workerapi.CommitActorTurnResponse, bool, error) {
	input, err := store.LockSessionTurnInput(ctx, db.LockSessionTurnInputParams{EnvironmentID: authority.run.EnvironmentID, SessionID: authority.actor.ID, ID: pgvalue.UUID(commit.turnID)})
	if err != nil {
		return workerapi.CommitActorTurnResponse{}, false, staleActorTurnCommit(err)
	}
	if input.Status != commit.disposition || input.TerminalRequestFingerprint.String != commit.fingerprint || input.RunID != authority.run.ID || input.AttemptNumber.Int32 != authority.attempt.Number || input.RunGeneration.Int64 != commit.generation || !input.TerminalEventID.Valid {
		return workerapi.CommitActorTurnResponse{}, false, nil
	}
	if authority.actor.CommittedInputSequence != commit.targetInputSequence ||
		authority.workspaceMount.MaterializedVersionID != authority.workspaceLease.BaseWorkspaceVersionID {
		return workerapi.CommitActorTurnResponse{}, false, nil
	}
	version, err := getActorTurnVersion(ctx, store, authority, authority.workspace.HeadVersionID)
	if err != nil {
		return workerapi.CommitActorTurnResponse{}, false, staleActorTurnCommit(err)
	}
	if version.ContentDigest.String != commit.tree.Digest || version.LogicalSizeBytes != commit.tree.SizeBytes ||
		version.EntryCount != int32(commit.tree.EntryCount) {
		return workerapi.CommitActorTurnResponse{}, false, nil
	}
	if commit.artifact == nil {
		if authority.workspace.HeadVersionID != pgvalue.UUID(commit.baseWorkspaceVersionID) {
			return workerapi.CommitActorTurnResponse{}, false, nil
		}
	} else if version.ParentVersionID != pgvalue.UUID(commit.baseWorkspaceVersionID) ||
		version.SourceWorkspaceLeaseID != authority.workspaceLease.ID ||
		version.OwnershipGeneration != authority.workspace.OwnershipGeneration ||
		version.WriterGeneration != authority.workspace.WriterGeneration ||
		!version.ArtifactRowKind.Valid || version.ArtifactRowKind.ArtifactKind != db.ArtifactKindWorkspaceVersion ||
		!version.ArtifactDigest.Valid || version.ArtifactDigest.String != commit.artifact.Digest ||
		!version.ArtifactSizeBytes.Valid || version.ArtifactSizeBytes.Int64 != commit.artifact.SizeBytes ||
		!version.ArtifactMediaType.Valid || version.ArtifactMediaType.String != commit.artifact.MediaType {
		return workerapi.CommitActorTurnResponse{}, false, nil
	}
	response, err := projectActorTurnResponse(request, commit, authority.workspace.HeadVersionID)
	response.EventID = pgvalue.UUIDString(input.TerminalEventID)
	return response, err == nil, err
}

func getActorTurnVersion(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	versionID pgtype.UUID,
) (db.GetWorkspaceResetTargetAuthorityRow, error) {
	return store.GetWorkspaceResetTargetAuthority(ctx, db.GetWorkspaceResetTargetAuthorityParams{
		OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
		EnvironmentID: authority.run.EnvironmentID, WorkspaceID: authority.workspace.ID, VersionID: versionID,
	})
}

func projectActorTurnResponse(
	request workerapi.CommitActorTurnRequest,
	commit parsedActorTurnCommit,
	versionID pgtype.UUID,
) (workerapi.CommitActorTurnResponse, error) {
	workspaceVersionID, err := requiredClaimUUIDString("actor turn workspace version ID", versionID)
	if err != nil {
		return workerapi.CommitActorTurnResponse{}, err
	}
	return workerapi.CommitActorTurnResponse{
		Lease: request.Lease, CorrelationID: commit.correlationID.String(),
		CommittedInputSequence: commit.targetInputSequence, WorkspaceVersionID: workspaceVersionID,
		Tree: request.Tree,
	}, nil
}

func staleActorTurnCommit(err error) error {
	var operation *session.OperationError
	if errors.As(err, &operation) {
		return errors.Join(errStaleActorTurnCommit, err)
	}
	if errors.Is(err, errStaleWorkerClaims) {
		return err
	}
	if err == nil || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errStaleRunLeaseClaim) ||
		errors.Is(err, errStaleRunFinalization) || errors.Is(err, errStaleActorCompletion) ||
		errors.Is(err, session.ErrTurnStopped) || errors.Is(err, session.ErrTurnNotActive) || errors.Is(err, session.ErrTurnScope) {
		return errStaleActorTurnCommit
	}
	return err
}
