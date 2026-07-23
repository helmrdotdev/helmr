package control

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleActorTurnCommit = errors.New("Actor turn commit is stale")

type parsedActorTurnCommit struct {
	lease               parsedRunLeaseReceipt
	correlationID       uuid.UUID
	targetInputSequence int64
	baseVersionID       uuid.UUID
	tree                workspace.TreeIdentity
	artifact            *api.WorkerWorkspaceArtifact
}

func parseActorTurnCommitRequest(request api.WorkerCommitActorTurnRequest) (parsedActorTurnCommit, error) {
	lease, err := parseRunLeaseReceipt(request.Lease)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	correlationID, err := parseCanonicalUUID("correlation_id", request.CorrelationID)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	baseVersionID, err := parseCanonicalUUID("base_workspace_version_id", request.BaseWorkspaceVersionID)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	if baseVersionID != lease.baseWorkspaceVersionID || request.BaseWorkspaceVersionID != request.Lease.BaseWorkspaceVersionID {
		return parsedActorTurnCommit{}, errors.New("base_workspace_version_id must match the Run Lease receipt")
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
		lease: lease, correlationID: correlationID, targetInputSequence: request.TargetInputSequence,
		baseVersionID: baseVersionID, tree: tree, artifact: request.Artifact,
	}, nil
}

func (s *Server) commitActorTurn(
	ctx context.Context,
	worker workerActor,
	request api.WorkerCommitActorTurnRequest,
	commit parsedActorTurnCommit,
) (api.WorkerCommitActorTurnResponse, error) {
	if request.Lease.WorkerEpoch != worker.WorkerEpoch || request.Lease.WorkerProtocolVersion != worker.ProtocolVersion {
		return api.WorkerCommitActorTurnResponse{}, errStaleActorTurnCommit
	}
	if commit.artifact != nil {
		capture := parsedTaskWorkspaceCapture{tree: commit.tree, artifact: *commit.artifact}
		if _, err := s.verifyTaskWorkspaceCapture(ctx, capture); err != nil {
			return api.WorkerCommitActorTurnResponse{}, err
		}
	}

	var response api.WorkerCommitActorTurnResponse
	err := s.inTx(ctx, func(work *txWork) error {
		if _, err := secret.LockAttemptDelivery(
			ctx, work.q, pgvalue.UUID(commit.lease.runID), request.Lease.AttemptNumber,
			pgvalue.UUID(commit.lease.workspaceID),
		); err != nil {
			return fmt.Errorf("lock Actor turn Secret authority: %w", err)
		}
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(commit.lease.leaseID), LeaseSequence: request.Lease.LeaseSequence,
			WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleActorTurnCommit(err)
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
		if authority.actor.CommittedInputSequence+1 != commit.targetInputSequence ||
			commit.targetInputSequence >= authority.actor.NextInputSequence ||
			authority.workspaceLease.BaseVersionID != pgvalue.UUID(commit.baseVersionID) {
			return errStaleActorTurnCommit
		}
		currentReceipt, err := projectActorTurnLease(authority)
		if err != nil {
			return err
		}
		if !equalRunLeaseReceipt(currentReceipt, request.Lease) {
			return errStaleActorTurnCommit
		}
		base, err := getActorTurnVersion(ctx, work.q, authority, authority.workspaceLease.BaseVersionID)
		if err != nil {
			return staleActorTurnCommit(err)
		}
		restoredBase := authority.workspaceLease.BaseVersionID != authority.workspace.HeadVersionID
		if restoredBase && (base.ParentVersionID != authority.workspace.HeadVersionID ||
			base.OwnershipGeneration != authority.workspace.OwnershipGeneration ||
			base.WriterGeneration != authority.workspace.WriterGeneration ||
			!authority.runtime.RestoreCheckpointID.Valid) {
			return errStaleActorTurnCommit
		}
		changed := commit.tree.Digest != base.ContentDigest ||
			commit.tree.SizeBytes != base.LogicalSizeBytes || commit.tree.EntryCount != int(base.EntryCount)
		if changed != (commit.artifact != nil) {
			return errStaleActorTurnCommit
		}
		committedAt, err := work.q.GetTaskCompletionTime(ctx)
		if err != nil || !committedAt.Valid {
			if err == nil {
				err = errors.New("database Actor turn commit time is unavailable")
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
					PrivateWorkspaceVersionID: authority.workspaceLease.BaseVersionID,
					TargetInputSequence:       commit.targetInputSequence,
				},
			); err != nil {
				return staleActorTurnCommit(err)
			}
		}

		versionID := authority.workspaceLease.BaseVersionID
		if changed {
			versionID, err = recordTaskWorkspaceVersion(
				ctx, work.q, worker, authority,
				parsedTaskWorkspaceCapture{tree: commit.tree, artifact: *commit.artifact}, committedAt,
			)
			if err != nil {
				return err
			}
		} else if restoredBase {
			if _, err := work.q.PublishRestoredActorCheckpointWorkspaceVersion(
				ctx, db.PublishRestoredActorCheckpointWorkspaceVersionParams{
					CommittedAt: committedAt, VersionID: authority.workspaceLease.BaseVersionID,
					WorkspaceID: authority.workspace.ID, ExpectedParentVersionID: authority.workspace.HeadVersionID,
					OwnershipGeneration: authority.workspace.OwnershipGeneration,
					WriterGeneration:    authority.workspace.WriterGeneration,
					RestoreCheckpointID: authority.runtime.RestoreCheckpointID,
					RunID:               authority.run.ID, AttemptNumber: authority.attempt.Number,
				},
			); err != nil {
				return staleActorTurnCommit(err)
			}
		}
		if versionID != authority.workspace.HeadVersionID {
			previousHeadVersionID := authority.workspace.HeadVersionID
			authority.workspace, err = work.q.AdvanceActorWorkspaceHead(ctx, db.AdvanceActorWorkspaceHeadParams{
				NewHeadVersionID: versionID, CompletedAt: committedAt, ID: authority.workspace.ID,
				OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
				EnvironmentID: authority.run.EnvironmentID, ActorID: authority.actor.ID,
				OwnershipGeneration:   authority.workspace.OwnershipGeneration,
				WriterGeneration:      authority.workspace.WriterGeneration,
				ExpectedHeadVersionID: previousHeadVersionID,
			})
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
					OwnerRunLeaseID: authority.runLease.ID, ExpectedVersionID: pgvalue.UUID(commit.baseVersionID),
					OwnershipGeneration:    authority.workspace.OwnershipGeneration,
					WriterGeneration:       authority.workspace.WriterGeneration,
					MountFencingGeneration: authority.workspaceMount.FencingGeneration,
				},
			)
			if err != nil {
				return staleActorTurnCommit(err)
			}
		}
		authority.actor, err = work.q.AdvanceActorTurnCursor(ctx, db.AdvanceActorTurnCursorParams{
			TargetInputSequence: commit.targetInputSequence, CommittedAt: committedAt,
			EnvironmentID: authority.run.EnvironmentID, ActorID: authority.actor.ID,
			WorkspaceID: authority.workspace.ID, RunID: authority.run.ID,
			ExpectedRunGeneration: authority.actor.RunGeneration,
			ExpectedInputSequence: commit.targetInputSequence - 1,
		})
		if err != nil {
			return staleActorTurnCommit(err)
		}
		response, err = projectActorTurnResponse(request, commit, authority, versionID)
		return err
	})
	return response, err
}

func validateActorTurnAuthority(ctx context.Context, store db.Querier, authority runLeaseClaimAuthority) error {
	actor := authority.actor
	if authority.run.EntrypointKind != "actor" || !authority.run.ActorID.Valid ||
		authority.run.ActorID != actor.ID || authority.run.ParentRunID.Valid ||
		authority.run.ParentOwnsLifecycle.Valid || authority.runLease.State != db.RunLeaseStateRunning ||
		!authority.run.ActiveStartedAt.Valid || !authority.attempt.EntrypointEnteredAt.Valid ||
		authority.attempt.TerminalAt.Valid || !authority.attempt.ActorStartInputSequence.Valid ||
		!authority.run.ActorStartInputSequence.Valid || !authority.run.ActorStartInputHighWatermark.Valid ||
		!actor.CurrentRunID.Valid || actor.CurrentRunID != authority.run.ID ||
		(actor.State != "open" && actor.State != "closing") ||
		authority.workspace.OwnerActorID != actor.ID || authority.workspace.OwnerRunID.Valid ||
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
	request api.WorkerCommitActorTurnRequest,
	commit parsedActorTurnCommit,
	authority runLeaseClaimAuthority,
) (api.WorkerCommitActorTurnResponse, bool, error) {
	if authority.actor.CommittedInputSequence != commit.targetInputSequence ||
		authority.workspaceMount.MaterializedVersionID != authority.workspaceLease.BaseVersionID {
		return api.WorkerCommitActorTurnResponse{}, false, nil
	}
	currentReceipt, err := projectActorTurnLease(authority)
	if err != nil {
		return api.WorkerCommitActorTurnResponse{}, false, err
	}
	replayReceipt := currentReceipt
	replayReceipt.BaseWorkspaceVersionID = request.BaseWorkspaceVersionID
	if !equalRunLeaseReceipt(replayReceipt, request.Lease) {
		return api.WorkerCommitActorTurnResponse{}, false, nil
	}
	version, err := getActorTurnVersion(ctx, store, authority, authority.workspace.HeadVersionID)
	if err != nil {
		return api.WorkerCommitActorTurnResponse{}, false, staleActorTurnCommit(err)
	}
	if version.ContentDigest != commit.tree.Digest || version.LogicalSizeBytes != commit.tree.SizeBytes ||
		version.EntryCount != int32(commit.tree.EntryCount) {
		return api.WorkerCommitActorTurnResponse{}, false, nil
	}
	if commit.artifact == nil {
		if authority.workspace.HeadVersionID != pgvalue.UUID(commit.baseVersionID) {
			return api.WorkerCommitActorTurnResponse{}, false, nil
		}
	} else if version.ParentVersionID != pgvalue.UUID(commit.baseVersionID) ||
		version.SourceWorkspaceLeaseID != authority.workspaceLease.ID ||
		version.OwnershipGeneration != authority.workspace.OwnershipGeneration ||
		version.WriterGeneration != authority.workspace.WriterGeneration ||
		!version.ArtifactKind.Valid || version.ArtifactKind.ArtifactKind != db.ArtifactKindWorkspaceVersion ||
		!version.ArtifactRowKind.Valid || version.ArtifactRowKind.ArtifactKind != db.ArtifactKindWorkspaceVersion ||
		!version.ArtifactDigest.Valid || version.ArtifactDigest.String != commit.artifact.Digest ||
		!version.ArtifactSizeBytes.Valid || version.ArtifactSizeBytes.Int64 != commit.artifact.SizeBytes ||
		!version.ArtifactMediaType.Valid || version.ArtifactMediaType.String != commit.artifact.MediaType {
		return api.WorkerCommitActorTurnResponse{}, false, nil
	}
	response, err := projectActorTurnResponse(request, commit, authority, authority.workspace.HeadVersionID)
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

func projectActorTurnLease(authority runLeaseClaimAuthority) (api.WorkerRunLeaseReceipt, error) {
	return projectRunLeaseReceipt(runLeaseProjectionAuthority{
		run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
		networkSlot: authority.networkSlot, runLease: authority.runLease,
		workspace: authority.workspace, workspaceMount: authority.workspaceMount,
		workspaceLease: authority.workspaceLease,
	})
}

func projectActorTurnResponse(
	request api.WorkerCommitActorTurnRequest,
	commit parsedActorTurnCommit,
	authority runLeaseClaimAuthority,
	versionID pgtype.UUID,
) (api.WorkerCommitActorTurnResponse, error) {
	lease, err := projectActorTurnLease(authority)
	if err != nil {
		return api.WorkerCommitActorTurnResponse{}, err
	}
	workspaceVersionID, err := requiredClaimUUIDString("Actor turn Workspace version ID", versionID)
	if err != nil {
		return api.WorkerCommitActorTurnResponse{}, err
	}
	return api.WorkerCommitActorTurnResponse{
		Lease: lease, RunID: request.Lease.RunID, AttemptNumber: request.Lease.AttemptNumber,
		RunLeaseID: request.Lease.ID, CorrelationID: commit.correlationID.String(),
		CommittedInputSequence: commit.targetInputSequence, WorkspaceVersionID: workspaceVersionID,
		Tree: request.Tree,
	}, nil
}

func staleActorTurnCommit(err error) error {
	if err == nil || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errStaleRunLeaseClaim) ||
		errors.Is(err, errStaleRunFinalization) {
		return errStaleActorTurnCommit
	}
	return err
}
