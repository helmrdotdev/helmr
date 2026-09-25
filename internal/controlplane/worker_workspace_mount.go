package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	workspaceMountReservationDuration = 5 * time.Minute
	workspaceExecLeaseRenewalTTL      = 30 * time.Minute
)

func (s *Server) workerClaimWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request workerapi.WorkspaceMountClaimRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace mount claim JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	channelToken, err := auth.GenerateOpaque(32)
	if err != nil {
		writeError(w, errors.New("generate workspace mount channel token"))
		return
	}
	row, err := s.db.ClaimWorkspaceMount(r.Context(), db.ClaimWorkspaceMountParams{
		WorkerInstanceID:           pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:                worker.WorkerEpoch,
		GuestChannelTokenHash:      guestChannelTokenHash(channelToken),
		GuestChannelTokenExpiresAt: pgvalue.Timestamptz(time.Now().Add(workspaceMountReservationDuration)),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, workerapi.WorkspaceMountClaimResponse{})
		return
	}
	if err != nil {
		writeError(w, errors.New("claim workspace mount"))
		return
	}
	mount := projectWorkerWorkspaceMount(row)
	mount.GuestdChannelToken = channelToken
	writeJSON(w, http.StatusOK, workerapi.WorkspaceMountClaimResponse{Mount: mount})
}

func (s *Server) workerRenewWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request workerapi.WorkspaceMountRenewRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace mount renewal JSON: %w", err)))
		return
	}
	params, err := s.workspaceMountTransition(r.Context(), request.OrgID, request.WorkspaceMountID)
	if err != nil {
		writeError(w, err)
		return
	}
	var mount db.WorkspaceMount
	err = s.inTx(r.Context(), func(work *txWork) error {
		row, err := work.q.RenewWorkspaceMount(r.Context(), db.RenewWorkspaceMountParams{
			GuestChannelTokenExpiresAt: pgvalue.Timestamptz(time.Now().Add(workspaceMountReservationDuration)),
			OrgID:                      params.orgID, ID: params.mount.ID,
			WorkerInstanceID: params.workerID, WorkerEpoch: params.epoch,
			RuntimeInstanceID: params.mount.RuntimeInstanceID,
		})
		if err != nil {
			return err
		}
		mount = row
		_, err = work.q.RenewWorkspaceExecLeaseForMount(
			r.Context(),
			db.RenewWorkspaceExecLeaseForMountParams{
				ExpiresAt:        pgvalue.Timestamptz(time.Now().Add(workspaceExecLeaseRenewalTTL)),
				WorkspaceMountID: mount.ID,
			},
		)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("workspace mount is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("renew workspace mount"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceMountResponse(mount))
}

func (s *Server) workerMarkWorkspaceMountMounted(w http.ResponseWriter, r *http.Request) {
	var request workerapi.WorkspaceMountMountedRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid mounted workspace JSON: %w", err)))
		return
	}
	params, err := s.workspaceMountTransition(r.Context(), request.OrgID, request.WorkspaceMountID)
	if err != nil {
		writeError(w, err)
		return
	}
	mount, err := s.db.MarkWorkspaceMountMounted(r.Context(), db.MarkWorkspaceMountMountedParams{
		OrgID: params.orgID, ID: params.mount.ID,
		WorkerInstanceID: params.workerID, WorkerEpoch: params.epoch,
		RuntimeInstanceID: params.mount.RuntimeInstanceID,
		FencingGeneration: params.mount.FencingGeneration,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("workspace mount is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("mark workspace mount mounted"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceMountResponse(mount))
}

func (s *Server) workerCaptureWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request workerapi.WorkspaceMountCaptureRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if request.Computer.ComputerID == "" || request.Computer.LogicalBytes != request.Computer.Root.LogicalBytes || request.Computer.Root.Validate(request.Computer.LogicalBytes) != nil {
		writeError(w, badRequest(errors.New("invalid Computer generation")))
		return
	}
	worker := workerFromContext(r.Context())
	var versionID pgtype.UUID
	err := s.withExecComputerPublication(r.Context(), worker, request.OrgID, request.WorkspaceMountID, func(tx pgx.Tx, q db.Querier, a db.LockWorkspaceExecWorkerAuthorityRow) error {
		if request.Computer.ComputerID != pgvalue.UUIDString(a.WorkspaceProcess.WorkspaceID) {
			return conflict(errors.New("captured Computer differs from exec"))
		}
		if err := requireRuntimeComputerRoot(r.Context(), q, a.RuntimeInstance, a.WorkspaceProcess.EnvironmentID, a.WorkspaceProcess.WorkspaceID, computerPublicationKey("exec", a.WorkspaceProcess.ID, a.WorkspaceProcess.ID), request.Computer.Root); err != nil {
			return fmt.Errorf("require exec root: %w", err)
		}
		raw, err := json.Marshal(request.Computer.Root)
		if err != nil {
			return err
		}
		if a.WorkspaceProcess.StagedVersionID.Valid {
			var matches bool
			if err = tx.QueryRow(r.Context(), `SELECT locator=$4::jsonb FROM computer_version_roots WHERE environment_id=$1 AND computer_id=$2 AND version_id=$3`, a.WorkspaceProcess.EnvironmentID, a.WorkspaceProcess.WorkspaceID, a.WorkspaceProcess.StagedVersionID, raw).Scan(&matches); err != nil {
				return err
			}
			if !matches {
				return conflict(errors.New("computer capture replay differs"))
			}
			versionID = a.WorkspaceProcess.StagedVersionID
		} else {
			staged, err := q.StageWorkspaceExecCapture(r.Context(), db.StageWorkspaceExecCaptureParams{WorkspaceMountID: a.WorkspaceMount.ID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch, WorkspaceVersionID: pgvalue.UUID(uuid.NewV7()), LogicalBytes: request.Computer.LogicalBytes, RootPackDigest: pgvalue.Text(request.Computer.Root.Pack.Digest)})
			if err != nil {
				return fmt.Errorf("stage exec root: %w", err)
			}
			if err = q.CreateComputerVersionRoot(r.Context(), db.CreateComputerVersionRootParams{EnvironmentID: a.WorkspaceProcess.EnvironmentID, ComputerID: a.WorkspaceProcess.WorkspaceID, VersionID: staged.ID, Locator: raw}); err != nil {
				return err
			}
			versionID = staged.ID
		}
		return nil
	})
	if err != nil {
		s.writeRunComputerObjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.WorkspaceMountCaptureResponse{VersionID: pgvalue.UUIDString(versionID)})
}

func (s *Server) workerStopWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request workerapi.WorkspaceMountStopRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace stop JSON: %w", err)))
		return
	}
	if err := validateRuntimeClosedCleanupProof(request.CleanupProof, time.Now()); err != nil {
		writeError(w, badRequest(err))
		return
	}
	cleanupProof, err := json.Marshal(request.CleanupProof)
	if err != nil {
		writeError(w, badRequest(errors.New("encode runtime cleanup proof")))
		return
	}
	params, err := s.workspaceMountTransition(r.Context(), request.OrgID, request.WorkspaceMountID)
	if err != nil {
		writeError(w, err)
		return
	}
	if params.mount.Status == "unmounted" {
		writeJSON(w, http.StatusOK, workspaceMountResponse(params.mount))
		return
	}
	var finalAuthority *db.LockWorkspaceExecWorkerAuthorityRow
	var stopped db.WorkspaceMount
	err = s.inTx(r.Context(), func(work *txWork) error {
		locator, locatorErr := work.q.GetWorkspaceExecLocatorForMount(
			r.Context(),
			db.GetWorkspaceExecLocatorForMountParams{
				OrgID: params.orgID, WorkspaceMountID: params.mount.ID,
			},
		)
		if locatorErr == nil {
			secretsValid, err := lockWorkspaceExecPublicationSecrets(
				r.Context(),
				work.q,
				locator.ID,
				params.mount.WorkspaceID,
			)
			if err != nil {
				return err
			}
			if _, err := work.q.LockWorkspaceExecFailureWorkspace(
				r.Context(),
				db.LockWorkspaceExecFailureWorkspaceParams{
					OrgID:       params.orgID,
					WorkspaceID: params.mount.WorkspaceID,
				},
			); err != nil {
				return err
			}
			authority, err := work.q.LockWorkspaceExecWorkerAuthority(
				r.Context(),
				db.LockWorkspaceExecWorkerAuthorityParams{
					OrgID: params.orgID, ProcessID: locator.ID,
					WorkspaceMountID: params.mount.ID,
					WorkerInstanceID: params.workerID, WorkerEpoch: params.epoch,
					ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds,
				},
			)
			if err != nil {
				return err
			}
			finalAuthority = &authority
			if err := s.finalizeWorkspaceExec(
				r.Context(),
				work,
				authority,
				secretsValid,
			); err != nil {
				return err
			}
		} else if !errors.Is(locatorErr, pgx.ErrNoRows) {
			return locatorErr
		}
		row, err := work.q.StopWorkspaceMount(r.Context(), db.StopWorkspaceMountParams{
			ReasonCode: pgvalue.Text("worker_unmounted"),
			OrgID:      params.orgID, ID: params.mount.ID,
			WorkerInstanceID: params.workerID, WorkerEpoch: params.epoch,
			RuntimeInstanceID: params.mount.RuntimeInstanceID,
			FencingGeneration: params.mount.FencingGeneration,
			CleanupProof:      cleanupProof,
		})
		if err != nil {
			return err
		}
		stopped = db.WorkspaceMount(row)
		if finalAuthority != nil {
			return checkExecPublicationDeadline(r.Context(), work.q, *finalAuthority)
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("workspace stop is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("stop workspace mount"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceMountResponse(stopped))
}

func (s *Server) finalizeWorkspaceExec(
	ctx context.Context,
	work *txWork,
	authority db.LockWorkspaceExecWorkerAuthorityRow,
	secretsValid bool,
) error {
	mount := authority.WorkspaceMount
	process := authority.WorkspaceProcess
	lease := authority.WorkspaceLease
	var versionID pgtype.UUID
	finalState := db.WorkspaceProcessStatusFailed
	reasonCode := mount.FinalizationReasonCode
	errorJSON := mount.FinalizationError
	if mount.FinalizationAction.String == "capture" {
		if !process.StagedVersionID.Valid {
			return errors.New("workspace exec capture is not staged")
		}
		if secretsValid {
			if _, err := work.q.CommitStagedWorkspaceExecVersion(
				ctx,
				db.CommitStagedWorkspaceExecVersionParams{
					VersionID:   process.StagedVersionID,
					WorkspaceID: mount.WorkspaceID,
				},
			); err != nil {
				return err
			}
			versionID = process.StagedVersionID
			finalState = db.WorkspaceProcessStatusExited
		} else {
			affected, err := work.q.DiscardStagedWorkspaceExecVersion(
				ctx,
				db.DiscardStagedWorkspaceExecVersionParams{
					VersionID:   process.StagedVersionID,
					WorkspaceID: mount.WorkspaceID,
				},
			)
			if err != nil {
				return err
			}
			if affected != 1 {
				return errors.New("revoked workspace exec version is not discardable")
			}
			reasonCode = pgvalue.Text("workspace_exec_secret_revoked")
			errorJSON, err = json.Marshal(map[string]string{
				"code": "workspace_exec_secret_revoked",
			})
			if err != nil {
				return err
			}
		}
	}
	if _, err := work.q.FinalizeWorkspaceExecWorkspace(
		ctx,
		db.FinalizeWorkspaceExecWorkspaceParams{
			VersionID:             versionID,
			RestoreDesiredState:   process.RestoreDesiredState,
			WorkspaceID:           process.WorkspaceID,
			ExpectedHeadVersionID: authority.SavedHeadVersionID,
			OwnershipGeneration:   lease.OwnershipGeneration,
			WriterGeneration:      lease.WriterGeneration,
		},
	); err != nil {
		return err
	}
	finalized, err := work.q.FinalizeWorkspaceExecProcess(
		ctx,
		db.FinalizeWorkspaceExecProcessParams{
			Status:           finalState,
			ReasonCode:       reasonCode,
			Error:            errorJSON,
			ProcessID:        process.ID,
			WorkspaceMountID: mount.ID,
		},
	)
	if err != nil {
		return err
	}
	if _, err := work.q.ReleaseWorkspaceExecLease(
		ctx,
		db.ReleaseWorkspaceExecLeaseParams{
			LeaseID:   lease.ID,
			ProcessID: process.ID,
		},
	); err != nil {
		return err
	}
	claim, err := work.q.GetIdempotencyClaim(ctx, db.GetIdempotencyClaimParams{
		EnvironmentID: process.EnvironmentID,
		ID:            process.ClaimID,
	})
	if err != nil {
		return err
	}
	if claim.RetiredAt.Valid {
		return nil
	}
	receipt, err := json.Marshal(map[string]string{
		"process_id":  pgvalue.MustUUIDValue(finalized.ID).String(),
		"reason_code": reasonCode.String,
	})
	if err != nil {
		return err
	}
	claims, err := idempotency.TransactionForQueries(work.q)
	if err != nil {
		return err
	}
	if finalState == db.WorkspaceProcessStatusExited {
		_, err = claims.Complete(ctx, claim, receipt)
	} else {
		_, err = claims.Fail(ctx, claim, receipt)
	}
	return err
}

func lockWorkspaceExecPublicationSecrets(
	ctx context.Context,
	q interface {
		LockProcessSecretDelivery(
			context.Context,
			db.LockProcessSecretDeliveryParams,
		) ([]db.LockProcessSecretDeliveryRow, error)
	},
	processID pgtype.UUID,
	workspaceID pgtype.UUID,
) (bool, error) {
	rows, err := q.LockProcessSecretDelivery(
		ctx,
		db.LockProcessSecretDeliveryParams{
			ProcessID:   processID,
			WorkspaceID: workspaceID,
		},
	)
	if err != nil {
		return false, err
	}
	if len(rows) > workspace.MaxSecretPlacements {
		return false, errors.New("workspace secret placements exceed their bound")
	}
	return workspaceExecPublicationSecretsValid(rows, processID), nil
}

func workspaceExecPublicationSecretsValid(
	rows []db.LockProcessSecretDeliveryRow,
	processID pgtype.UUID,
) bool {
	for _, row := range rows {
		if row.Secret.Status != "active" ||
			!row.ResolutionID.Valid ||
			!row.ResolutionProcessID.Valid ||
			row.ResolutionProcessID != processID ||
			!row.ResolutionSecretVersionID.Valid ||
			!row.ResolutionRevocationGeneration.Valid ||
			row.ResolutionRevocationGeneration.Int64 !=
				row.Secret.RevocationGeneration {
			return false
		}
	}
	return true
}

func (s *Server) workerFailWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request workerapi.WorkspaceMountFailRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace mount failure JSON: %w", err)))
		return
	}
	errorJSON, err := normalizedJSONObject(request.Error, "error")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	params, err := s.workspaceMountTransition(r.Context(), request.OrgID, request.WorkspaceMountID)
	if err != nil {
		writeError(w, err)
		return
	}
	var failed db.WorkspaceMount
	err = s.inTx(r.Context(), func(work *txWork) error {
		var execAuthority *db.LockWorkspaceExecFailureAuthorityRow
		locator, locatorErr := work.q.GetWorkspaceExecLocatorForMount(
			r.Context(),
			db.GetWorkspaceExecLocatorForMountParams{
				OrgID: params.orgID, WorkspaceMountID: params.mount.ID,
			},
		)
		if locatorErr == nil {
			if _, err := work.q.LockWorkspaceExecFailureWorkspace(
				r.Context(),
				db.LockWorkspaceExecFailureWorkspaceParams{
					OrgID: params.orgID, WorkspaceID: locator.WorkspaceID,
				},
			); err != nil {
				return err
			}
			authority, err := work.q.LockWorkspaceExecFailureAuthority(
				r.Context(),
				db.LockWorkspaceExecFailureAuthorityParams{
					OrgID: params.orgID, ProcessID: locator.ID,
					WorkspaceMountID: params.mount.ID,
					WorkerInstanceID: params.workerID, WorkerEpoch: params.epoch,
				},
			)
			if err != nil {
				return err
			}
			execAuthority = &authority
		} else if !errors.Is(locatorErr, pgx.ErrNoRows) {
			return locatorErr
		}
		row, err := work.q.FailWorkspaceMount(r.Context(), db.FailWorkspaceMountParams{
			ReasonCode: pgvalue.Text("worker_mount_failed"), Error: errorJSON,
			OrgID: params.orgID, ID: params.mount.ID,
			WorkerInstanceID: params.workerID, WorkerEpoch: params.epoch,
			RuntimeInstanceID: params.mount.RuntimeInstanceID,
			FencingGeneration: params.mount.FencingGeneration,
		})
		if err != nil {
			return err
		}
		failed = db.WorkspaceMount(row)
		if execAuthority != nil {
			return s.failWorkspaceExec(
				r.Context(),
				work,
				*execAuthority,
				"worker_mount_failed",
				errorJSON,
			)
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("workspace mount is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("fail workspace mount"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceMountResponse(failed))
}

func (s *Server) failWorkspaceExec(
	ctx context.Context,
	work *txWork,
	authority db.LockWorkspaceExecFailureAuthorityRow,
	reasonCode string,
	errorJSON []byte,
) error {
	process := authority.WorkspaceProcess
	mount := authority.WorkspaceMount
	lease := authority.WorkspaceLease
	if process.StagedVersionID.Valid {
		affected, err := work.q.DiscardStagedWorkspaceExecVersion(
			ctx,
			db.DiscardStagedWorkspaceExecVersionParams{
				VersionID:   process.StagedVersionID,
				WorkspaceID: process.WorkspaceID,
			},
		)
		if err != nil {
			return err
		}
		if affected != 1 {
			return errors.New("staged workspace exec version is not discardable")
		}
	}
	if _, err := work.q.MarkWorkspaceExecRecoveryRequired(
		ctx,
		db.MarkWorkspaceExecRecoveryRequiredParams{
			RecoveryID: pgvalue.UUID(uuid.NewV7()), RecoveryReason: pgvalue.Text(reasonCode),
			WorkspaceID:           process.WorkspaceID,
			ExpectedHeadVersionID: authority.SavedHeadVersionID,
			OwnershipGeneration:   lease.OwnershipGeneration,
			WriterGeneration:      lease.WriterGeneration,
		},
	); err != nil {
		return err
	}
	failed, err := work.q.FailWorkspaceExecProcess(
		ctx,
		db.FailWorkspaceExecProcessParams{
			ReasonCode:       pgvalue.Text(reasonCode),
			Error:            errorJSON,
			ProcessID:        process.ID,
			WorkspaceMountID: mount.ID,
		},
	)
	if err != nil {
		return err
	}
	if _, err := work.q.ReleaseWorkspaceExecLease(
		ctx,
		db.ReleaseWorkspaceExecLeaseParams{
			LeaseID: lease.ID, ProcessID: process.ID,
		},
	); err != nil {
		return err
	}
	claim, err := work.q.GetIdempotencyClaim(ctx, db.GetIdempotencyClaimParams{
		EnvironmentID: process.EnvironmentID,
		ID:            process.ClaimID,
	})
	if err != nil {
		return err
	}
	if claim.RetiredAt.Valid {
		return nil
	}
	receipt, err := json.Marshal(map[string]string{
		"process_id":  pgvalue.MustUUIDValue(failed.ID).String(),
		"reason_code": reasonCode,
	})
	if err != nil {
		return err
	}
	claims, err := idempotency.TransactionForQueries(work.q)
	if err != nil {
		return err
	}
	_, err = claims.Fail(ctx, claim, receipt)
	return err
}

type workspaceMountTransitionAuthority struct {
	orgID    pgtype.UUID
	workerID pgtype.UUID
	epoch    int64
	mount    db.WorkspaceMount
}

func (s *Server) workspaceMountTransition(
	ctx context.Context,
	rawOrgID string,
	rawMountID string,
) (workspaceMountTransitionAuthority, error) {
	orgID, mountID, err := parseWorkspaceWorkerIDs(rawOrgID, rawMountID)
	if err != nil {
		return workspaceMountTransitionAuthority{}, err
	}
	worker := workerFromContext(ctx)
	mount, err := s.db.GetWorkspaceMountForWorker(
		ctx,
		db.GetWorkspaceMountForWorkerParams{
			OrgID: orgID, ID: mountID,
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:      worker.WorkerEpoch,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspaceMountTransitionAuthority{}, conflict(errors.New("workspace mount is stale"))
	}
	if err != nil {
		return workspaceMountTransitionAuthority{}, err
	}
	return workspaceMountTransitionAuthority{
		orgID: orgID, workerID: pgvalue.UUID(worker.WorkerInstanceID),
		epoch: worker.WorkerEpoch, mount: mount,
	}, nil
}

func parseWorkspaceWorkerIDs(rawOrgID, rawMountID string) (pgtype.UUID, pgtype.UUID, error) {
	orgID, err := ids.Parse(rawOrgID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, badRequest(errors.New("org_id must be a canonical UUIDv7"))
	}
	mountID, err := ids.Parse(rawMountID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, badRequest(errors.New("workspace_mount_id must be a canonical UUIDv7"))
	}
	return pgvalue.UUID(orgID), pgvalue.UUID(mountID), nil
}

func guestChannelTokenHash(value string) string {
	return sha256sum.HexBytes([]byte(strings.TrimSpace(value)))
}

func workspaceMountResponse(row db.WorkspaceMount) workerapi.WorkspaceMountResponse {
	response := workerapi.WorkspaceMountResponse{
		ID:                     pgvalue.MustUUIDValue(row.ID).String(),
		ProjectID:              pgvalue.MustUUIDValue(row.ProjectID).String(),
		EnvironmentID:          pgvalue.MustUUIDValue(row.EnvironmentID).String(),
		WorkspaceID:            pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		BaseWorkspaceVersionID: pgvalue.MustUUIDValue(row.MaterializedVersionID).String(),
		WorkerInstanceID:       pgvalue.MustUUIDValue(row.WorkerInstanceID).String(),
		Status:                 string(row.Status),
		FencingGeneration:      row.FencingGeneration,
		DirtyGeneration:        row.DirtyGeneration,
		FinalizationKind:       row.FinalizationAction.String,
		ReservationExpiresAt:   pgTime(row.GuestChannelTokenExpiresAt),
		LastHeartbeatAt:        pgTime(row.UpdatedAt),
		CreatedAt:              row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
	return response
}

func pgTime(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time
	return &result
}

func projectWorkerWorkspaceMount(row db.ClaimWorkspaceMountRow) *workerapi.WorkspaceMount {
	target := workerapi.ComputerMountTarget{BaseWorkspaceVersionID: pgvalue.MustUUIDValue(row.MaterializedVersionID).String()}
	return &workerapi.WorkspaceMount{
		ID:                     pgvalue.MustUUIDValue(row.ID).String(),
		OrgID:                  pgvalue.MustUUIDValue(row.OrgID).String(),
		ProjectID:              pgvalue.MustUUIDValue(row.ProjectID).String(),
		EnvironmentID:          pgvalue.MustUUIDValue(row.EnvironmentID).String(),
		WorkspaceID:            pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		DeploymentDefinitionID: pgvalue.MustUUIDValue(row.DeploymentDefinitionID).String(),
		Target:                 target,
		RuntimeInstanceID:      pgvalue.MustUUIDValue(row.RuntimeInstanceID).String(),
		RestoreCheckpointID:    pgvalue.UUIDString(row.RestoreCheckpointID),
		RestoreSourceVersionID: pgvalue.UUIDString(row.RestoreSourceVersionID),
		RuntimeEpoch:           row.WorkerEpoch,
		GuestdChannelTokenHash: row.GuestChannelTokenHash,
		Status:                 string(row.Status), RuntimeIdentityID: row.RuntimeID,
		WorkspaceImage: workerapi.CASObject{
			Digest: row.ImageArtifactDigest, SizeBytes: row.ImageArtifactSizeBytes,
			MediaType: row.ImageArtifactMediaType,
		},
		RootfsDigest:            row.RootfsDigest,
		WorkspaceMountPath:      "/workspace",
		RequestedMilliCPU:       row.ReservedCPUMillis,
		RequestedMemoryMiB:      row.ReservedMemoryBytes / (1024 * 1024),
		RequestedDiskMiB:        row.ReservedGuestEphemeralDiskBytes / (1024 * 1024),
		RequestedExecutionSlots: row.ReservedExecutionSlots,
		VMRuntimeContract:       row.VMRuntimeContract,
		FencingGeneration:       row.FencingGeneration,
		ExpiresAt:               row.GuestChannelTokenExpiresAt.Time,
	}
}
