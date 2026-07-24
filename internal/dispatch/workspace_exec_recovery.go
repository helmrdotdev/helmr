package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type RecoverableWorkspaceExecCandidate struct {
	OrgID                pgtype.UUID
	ProcessID            pgtype.UUID
	WorkspaceID          pgtype.UUID
	ExpectedStateVersion int64
}

type workspaceExecRecoveryKind uint8

const (
	workspaceExecRecoveryUncertain workspaceExecRecoveryKind = iota
	workspaceExecRecoveryCapture
	workspaceExecRecoveryDiscard
	workspaceExecRecoveryRevoked
)

func (d *Authority) RecoverWorkspaceExec(
	ctx context.Context,
	candidate RecoverableWorkspaceExecCandidate,
) error {
	tx, err := d.begin(ctx)
	if err != nil {
		return fmt.Errorf("begin Workspace exec recovery: %w", err)
	}
	defer rollback(ctx, tx)

	q := db.New(tx)
	secretsValid, err := lockWorkspaceExecRecoverySecrets(ctx, q, candidate)
	if err != nil {
		return err
	}
	if _, err := q.LockWorkspaceExecFailureWorkspace(
		ctx,
		db.LockWorkspaceExecFailureWorkspaceParams{
			OrgID:       candidate.OrgID,
			WorkspaceID: candidate.WorkspaceID,
		},
	); err != nil {
		return classifyWorkspaceExecRecoveryError(err)
	}
	authority, err := q.LockWorkspaceExecRecoveryAuthority(
		ctx,
		db.LockWorkspaceExecRecoveryAuthorityParams{
			OrgID:                candidate.OrgID,
			ProcessID:            candidate.ProcessID,
			WorkspaceID:          candidate.WorkspaceID,
			ExpectedStateVersion: candidate.ExpectedStateVersion,
		},
	)
	if err != nil {
		return classifyWorkspaceExecRecoveryError(err)
	}
	claim, err := q.GetIdempotencyClaim(
		ctx,
		db.GetIdempotencyClaimParams{
			EnvironmentID: authority.WorkspaceProcess.EnvironmentID,
			ID:            authority.WorkspaceProcess.ClaimID,
		},
	)
	if err != nil {
		return classifyWorkspaceExecRecoveryError(err)
	}

	reasonCode := "workspace_exec_lease_expired"
	if authority.WorkspaceMount.State == db.WorkspaceMountStateLost {
		reasonCode = "workspace_exec_worker_lost"
	} else if _, err := q.LoseWorkspaceExecMount(
		ctx,
		db.LoseWorkspaceExecMountParams{
			ReasonCode:       pgvalue.Text(reasonCode),
			WorkspaceMountID: authority.WorkspaceMount.ID,
			WorkspaceID:      authority.WorkspaceProcess.WorkspaceID,
		},
	); err != nil {
		return classifyWorkspaceExecRecoveryError(err)
	}
	if affected, err := q.CloseWorkspaceExecRuntime(
		ctx,
		db.CloseWorkspaceExecRuntimeParams{
			ReasonCode:        reasonCode,
			RuntimeInstanceID: authority.WorkspaceMount.RuntimeInstanceID,
			WorkspaceID:       authority.WorkspaceProcess.WorkspaceID,
		},
	); err != nil {
		return fmt.Errorf("close recovered Workspace exec runtime: %w", err)
	} else if affected > 1 {
		return errors.New("Workspace exec recovery closed multiple runtimes")
	}

	switch classifyWorkspaceExecRecovery(authority, secretsValid) {
	case workspaceExecRecoveryCapture:
		if err := finalizeRecoveredWorkspaceExec(
			ctx,
			q,
			authority,
			claim,
			db.WorkspaceProcessStateExited,
			authority.WorkspaceMount.StagedVersionID,
			authority.WorkspaceMount.FinalizationReasonCode,
			authority.WorkspaceMount.FinalizationError,
		); err != nil {
			return err
		}
	case workspaceExecRecoveryDiscard:
		if err := finalizeRecoveredWorkspaceExec(
			ctx,
			q,
			authority,
			claim,
			db.WorkspaceProcessStateFailed,
			pgtype.UUID{},
			authority.WorkspaceMount.FinalizationReasonCode,
			authority.WorkspaceMount.FinalizationError,
		); err != nil {
			return err
		}
	case workspaceExecRecoveryRevoked:
		if err := failRevokedRecoveredWorkspaceExec(
			ctx,
			q,
			authority,
			claim,
		); err != nil {
			return err
		}
	default:
		if err := failUncertainWorkspaceExec(
			ctx,
			q,
			authority,
			claim,
			reasonCode,
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Workspace exec recovery: %w", err)
	}
	return nil
}

func classifyWorkspaceExecRecovery(
	authority db.LockWorkspaceExecRecoveryAuthorityRow,
	secretsValid bool,
) workspaceExecRecoveryKind {
	if authority.WorkspaceProcess.State != db.WorkspaceProcessStateExitRequested ||
		!authority.WorkspaceMount.FinalizationKind.Valid ||
		!authority.WorkspaceMount.FinalizationReasonCode.Valid ||
		authority.WorkspaceMount.FinalizationReasonCode.String == "" {
		return workspaceExecRecoveryUncertain
	}
	switch authority.WorkspaceMount.FinalizationKind.String {
	case "capture":
		if authority.WorkspaceMount.StagedVersionID.Valid &&
			len(authority.WorkspaceMount.FinalizationError) == 0 {
			if !secretsValid {
				return workspaceExecRecoveryRevoked
			}
			return workspaceExecRecoveryCapture
		}
	case "discard":
		if !authority.WorkspaceMount.StagedVersionID.Valid {
			return workspaceExecRecoveryDiscard
		}
	}
	return workspaceExecRecoveryUncertain
}

func lockWorkspaceExecRecoverySecrets(
	ctx context.Context,
	q *db.Queries,
	candidate RecoverableWorkspaceExecCandidate,
) (bool, error) {
	rows, err := q.LockProcessSecretDelivery(
		ctx,
		db.LockProcessSecretDeliveryParams{
			ProcessID:   candidate.ProcessID,
			WorkspaceID: candidate.WorkspaceID,
		},
	)
	if err != nil {
		return false, err
	}
	if len(rows) > 64 {
		return false, errors.New("Workspace Secret placements exceed their bound")
	}
	for _, row := range rows {
		if row.Secret.State != "active" ||
			!row.ResolutionID.Valid ||
			!row.ResolutionProcessID.Valid ||
			row.ResolutionProcessID != candidate.ProcessID ||
			!row.ResolutionSecretVersionID.Valid ||
			!row.ResolutionRevocationGeneration.Valid ||
			row.ResolutionRevocationGeneration.Int64 !=
				row.Secret.RevocationGeneration {
			return false, nil
		}
	}
	return true, nil
}

func finalizeRecoveredWorkspaceExec(
	ctx context.Context,
	q *db.Queries,
	authority db.LockWorkspaceExecRecoveryAuthorityRow,
	claim db.IdempotencyClaim,
	finalState db.WorkspaceProcessState,
	versionID pgtype.UUID,
	reasonCode pgtype.Text,
	errorJSON []byte,
) error {
	mount := authority.WorkspaceMount
	process := authority.WorkspaceProcess
	lease := authority.WorkspaceLease
	if finalState == db.WorkspaceProcessStateExited {
		if _, err := q.CommitStagedWorkspaceExecVersion(
			ctx,
			db.CommitStagedWorkspaceExecVersionParams{
				VersionID:   versionID,
				WorkspaceID: process.WorkspaceID,
			},
		); err != nil {
			return classifyWorkspaceExecRecoveryError(err)
		}
	}
	if _, err := q.FinalizeWorkspaceExecWorkspace(
		ctx,
		db.FinalizeWorkspaceExecWorkspaceParams{
			VersionID:           versionID,
			RestoreDesiredState: process.RestoreDesiredState,
			WorkspaceID:         process.WorkspaceID,
			BaseVersionID:       process.BaseVersionID,
			OwnershipGeneration: lease.OwnershipGeneration,
			WriterGeneration:    lease.WriterGeneration,
		},
	); err != nil {
		return classifyWorkspaceExecRecoveryError(err)
	}
	finalized, err := q.FinalizeWorkspaceExecProcess(
		ctx,
		db.FinalizeWorkspaceExecProcessParams{
			State:            finalState,
			ReasonCode:       reasonCode,
			Error:            errorJSON,
			ProcessID:        process.ID,
			WorkspaceMountID: mount.ID,
		},
	)
	if err != nil {
		return classifyWorkspaceExecRecoveryError(err)
	}
	switch lease.State {
	case db.WorkspaceLeaseStateActive, db.WorkspaceLeaseStateReleasing:
		if _, err := q.ReleaseWorkspaceExecLease(
			ctx,
			db.ReleaseWorkspaceExecLeaseParams{
				LeaseID:   lease.ID,
				ProcessID: process.ID,
			},
		); err != nil {
			return classifyWorkspaceExecRecoveryError(err)
		}
	case db.WorkspaceLeaseStateExpired:
	default:
		return ErrCandidateChanged
	}
	return finalizeRecoveredWorkspaceExecClaim(
		ctx,
		q,
		claim,
		finalized,
		finalState == db.WorkspaceProcessStateExited,
	)
}

func failRevokedRecoveredWorkspaceExec(
	ctx context.Context,
	q *db.Queries,
	authority db.LockWorkspaceExecRecoveryAuthorityRow,
	claim db.IdempotencyClaim,
) error {
	affected, err := q.DiscardStagedWorkspaceExecVersion(
		ctx,
		db.DiscardStagedWorkspaceExecVersionParams{
			VersionID:   authority.WorkspaceMount.StagedVersionID,
			WorkspaceID: authority.WorkspaceProcess.WorkspaceID,
		},
	)
	if err != nil {
		return fmt.Errorf("discard revoked Workspace exec version: %w", err)
	}
	if affected != 1 {
		return errors.New("revoked Workspace exec version is not discardable")
	}
	errorJSON, err := json.Marshal(map[string]string{
		"code": "workspace_exec_secret_revoked",
	})
	if err != nil {
		return err
	}
	return finalizeRecoveredWorkspaceExec(
		ctx,
		q,
		authority,
		claim,
		db.WorkspaceProcessStateFailed,
		pgtype.UUID{},
		pgvalue.Text("workspace_exec_secret_revoked"),
		errorJSON,
	)
}

func failUncertainWorkspaceExec(
	ctx context.Context,
	q *db.Queries,
	authority db.LockWorkspaceExecRecoveryAuthorityRow,
	claim db.IdempotencyClaim,
	reasonCode string,
) error {
	if authority.WorkspaceMount.StagedVersionID.Valid {
		affected, err := q.DiscardStagedWorkspaceExecVersion(
			ctx,
			db.DiscardStagedWorkspaceExecVersionParams{
				VersionID:   authority.WorkspaceMount.StagedVersionID,
				WorkspaceID: authority.WorkspaceProcess.WorkspaceID,
			},
		)
		if err != nil {
			return fmt.Errorf("discard recovered Workspace exec version: %w", err)
		}
		if affected != 1 {
			return errors.New("recovered Workspace exec version is not discardable")
		}
	}
	if _, err := q.MarkWorkspaceExecRecoveryRequired(
		ctx,
		db.MarkWorkspaceExecRecoveryRequiredParams{
			WorkspaceID:         authority.WorkspaceProcess.WorkspaceID,
			BaseVersionID:       authority.WorkspaceProcess.BaseVersionID,
			OwnershipGeneration: authority.WorkspaceLease.OwnershipGeneration,
			WriterGeneration:    authority.WorkspaceLease.WriterGeneration,
		},
	); err != nil {
		return classifyWorkspaceExecRecoveryError(err)
	}
	errorJSON, err := json.Marshal(map[string]string{"code": reasonCode})
	if err != nil {
		return err
	}
	failed, err := q.FailWorkspaceExecProcess(
		ctx,
		db.FailWorkspaceExecProcessParams{
			ReasonCode:       pgvalue.Text(reasonCode),
			Error:            errorJSON,
			ProcessID:        authority.WorkspaceProcess.ID,
			WorkspaceMountID: authority.WorkspaceMount.ID,
		},
	)
	if err != nil {
		return classifyWorkspaceExecRecoveryError(err)
	}
	switch authority.WorkspaceLease.State {
	case db.WorkspaceLeaseStateActive, db.WorkspaceLeaseStateReleasing:
		if _, err := q.ExpireWorkspaceExecLease(
			ctx,
			db.ExpireWorkspaceExecLeaseParams{
				ReasonCode: pgvalue.Text(reasonCode),
				LeaseID:    authority.WorkspaceLease.ID,
				ProcessID:  authority.WorkspaceProcess.ID,
			},
		); err != nil {
			return classifyWorkspaceExecRecoveryError(err)
		}
	case db.WorkspaceLeaseStateExpired:
	default:
		return ErrCandidateChanged
	}
	return finalizeRecoveredWorkspaceExecClaim(
		ctx,
		q,
		claim,
		failed,
		false,
	)
}

func finalizeRecoveredWorkspaceExecClaim(
	ctx context.Context,
	q *db.Queries,
	claim db.IdempotencyClaim,
	process db.WorkspaceProcess,
	complete bool,
) error {
	if claim.RetiredAt.Valid {
		return nil
	}
	receipt, err := json.Marshal(map[string]string{
		"process_id": pgvalue.MustUUIDValue(process.ID).String(),
		"reason_code": func() string {
			if process.TerminalReasonCode.Valid {
				return process.TerminalReasonCode.String
			}
			return ""
		}(),
	})
	if err != nil {
		return err
	}
	if complete {
		if _, err := q.CompleteIdempotencyClaim(
			ctx,
			db.CompleteIdempotencyClaimParams{
				Receipt:            receipt,
				EnvironmentID:      process.EnvironmentID,
				ID:                 process.ClaimID,
				RequestFingerprint: claim.RequestFingerprint,
			},
		); err != nil {
			return classifyWorkspaceExecRecoveryError(err)
		}
		return nil
	}
	if _, err := q.FailIdempotencyClaim(ctx, db.FailIdempotencyClaimParams{
		Receipt:            receipt,
		EnvironmentID:      process.EnvironmentID,
		ID:                 process.ClaimID,
		RequestFingerprint: claim.RequestFingerprint,
	}); err != nil {
		return classifyWorkspaceExecRecoveryError(err)
	}
	return nil
}

func classifyWorkspaceExecRecoveryError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCandidateChanged
	}
	return err
}
