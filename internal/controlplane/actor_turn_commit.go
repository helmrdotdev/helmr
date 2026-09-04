package controlplane

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleActorTurnCommit = errors.New("actor turn commit is stale")

type parsedActorTurnCommit struct {
	lease               parsedRunLeaseFence
	correlationID       uuid.UUID
	targetInputSequence int64
	baseVersionID       uuid.UUID
	tree                workspace.TreeIdentity
	artifact            *workerapi.WorkspaceArtifact
}

func parseActorTurnCommitRequest(request workerapi.CommitActorTurnRequest) (parsedActorTurnCommit, error) {
	lease, err := parseRunLeaseFence(request.Lease)
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
	request workerapi.CommitActorTurnRequest,
	commit parsedActorTurnCommit,
) (workerapi.CommitActorTurnResponse, error) {
	if commit.artifact != nil {
		capture := parsedTaskWorkspaceCapture{tree: commit.tree, artifact: *commit.artifact}
		if _, err := s.verifyTaskWorkspaceCapture(ctx, capture); err != nil {
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
		if authority.actor.CommittedInputSequence+1 != commit.targetInputSequence ||
			commit.targetInputSequence >= authority.actor.NextInputSequence ||
			authority.workspaceLease.BaseVersionID != pgvalue.UUID(commit.baseVersionID) {
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
				EnvironmentID: authority.run.EnvironmentID, SessionID: authority.actor.ID,
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
			EnvironmentID: authority.run.EnvironmentID, SessionID: authority.actor.ID,
			WorkspaceID: authority.workspace.ID, RunID: authority.run.ID,
			ExpectedRunGeneration: authority.actor.RunGeneration,
			ExpectedInputSequence: commit.targetInputSequence - 1,
		})
		if err != nil {
			return staleActorTurnCommit(err)
		}
		response, err = projectActorTurnResponse(request, commit, versionID)
		return err
	})
	return response, err
}

func validateActorTurnAuthority(ctx context.Context, store db.Querier, authority runLeaseClaimAuthority) error {
	actor := authority.actor
	if authority.run.EntrypointKind != "actor" || !authority.run.SessionID.Valid ||
		authority.run.SessionID != actor.ID || authority.run.ParentRunID.Valid ||
		authority.run.ParentOwnsLifecycle.Valid || authority.runLease.State != db.RunLeaseStateRunning ||
		!authority.run.ActiveStartedAt.Valid || !authority.attempt.EntrypointEnteredAt.Valid ||
		authority.attempt.TerminalAt.Valid || !authority.attempt.SessionInputStartSequence.Valid ||
		!authority.run.SessionInputStartSequence.Valid || !authority.run.SessionInputHighWatermark.Valid ||
		!actor.CurrentRunID.Valid || actor.CurrentRunID != authority.run.ID ||
		(actor.State != "open" && actor.State != "closing") ||
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
	if authority.actor.CommittedInputSequence != commit.targetInputSequence ||
		authority.workspaceMount.MaterializedVersionID != authority.workspaceLease.BaseVersionID {
		return workerapi.CommitActorTurnResponse{}, false, nil
	}
	version, err := getActorTurnVersion(ctx, store, authority, authority.workspace.HeadVersionID)
	if err != nil {
		return workerapi.CommitActorTurnResponse{}, false, staleActorTurnCommit(err)
	}
	if version.ContentDigest != commit.tree.Digest || version.LogicalSizeBytes != commit.tree.SizeBytes ||
		version.EntryCount != int32(commit.tree.EntryCount) {
		return workerapi.CommitActorTurnResponse{}, false, nil
	}
	if commit.artifact == nil {
		if authority.workspace.HeadVersionID != pgvalue.UUID(commit.baseVersionID) {
			return workerapi.CommitActorTurnResponse{}, false, nil
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
		return workerapi.CommitActorTurnResponse{}, false, nil
	}
	response, err := projectActorTurnResponse(request, commit, authority.workspace.HeadVersionID)
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
	if err == nil || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errStaleRunLeaseClaim) ||
		errors.Is(err, errStaleRunFinalization) {
		return errStaleActorTurnCommit
	}
	return err
}
