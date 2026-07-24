package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	workspaceMountReservationDuration = 5 * time.Minute
	workspaceExecLeaseRenewalTTL      = 30 * time.Minute
)

func (s *Server) workerClaimWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMountClaimRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Workspace mount claim JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	channelToken, err := token.GenerateOpaque(32)
	if err != nil {
		writeError(w, errors.New("generate Workspace mount channel token"))
		return
	}
	row, err := s.db.ClaimWorkspaceMount(r.Context(), db.ClaimWorkspaceMountParams{
		WorkerInstanceID:           pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:                worker.WorkerEpoch,
		GuestChannelTokenHash:      guestChannelTokenHash(channelToken),
		GuestChannelTokenExpiresAt: pgvalue.Timestamptz(time.Now().Add(workspaceMountReservationDuration)),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, api.WorkerWorkspaceMountClaimResponse{})
		return
	}
	if err != nil {
		writeError(w, errors.New("claim Workspace mount"))
		return
	}
	mount, err := projectWorkerWorkspaceMount(row)
	if err != nil {
		writeError(w, errors.New("project Workspace mount"))
		return
	}
	mount.GuestdChannelToken = channelToken
	writeJSON(w, http.StatusOK, api.WorkerWorkspaceMountClaimResponse{Mount: mount})
}

func (s *Server) workerRenewWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMountRenewRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Workspace mount renewal JSON: %w", err)))
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
		writeError(w, conflict(errors.New("Workspace mount is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("renew Workspace mount"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceMountResponse(mount))
}

func (s *Server) workerMarkWorkspaceMountMounted(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMountMountedRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid mounted Workspace JSON: %w", err)))
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
		writeError(w, conflict(errors.New("Workspace mount is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("mark Workspace mount mounted"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceMountResponse(mount))
}

func (s *Server) workerCaptureWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMountCaptureRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Workspace capture JSON: %w", err)))
		return
	}
	params, err := s.workspaceMountTransition(r.Context(), request.OrgID, request.WorkspaceMountID)
	if err != nil {
		writeError(w, err)
		return
	}
	if strings.TrimSpace(request.ArtifactMediaType) != workspace.ArtifactMediaType ||
		strings.TrimSpace(request.ArtifactEncoding) != workspace.ArtifactEncoding ||
		request.ArtifactSizeBytes <= 0 || request.ArtifactEntryCount < 0 ||
		strings.TrimSpace(request.ArtifactDigest) == "" {
		writeError(w, badRequest(errors.New("Workspace capture artifact is invalid")))
		return
	}
	var versionID pgtype.UUID
	err = s.inTx(r.Context(), func(work *txWork) error {
		existing, err := work.q.GetStagedWorkspaceExecCapture(
			r.Context(),
			db.GetStagedWorkspaceExecCaptureParams{
				WorkspaceMountID: params.mount.ID,
				WorkerInstanceID: params.workerID,
				WorkerEpoch:      params.epoch,
			},
		)
		if err == nil {
			if existing.ContentDigest != strings.TrimSpace(request.ArtifactDigest) ||
				existing.SizeBytes != request.ArtifactSizeBytes ||
				existing.EntryCount != request.ArtifactEntryCount {
				return conflict(errors.New("Workspace capture replay differs"))
			}
			versionID = existing.ID
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := work.q.UpsertCasObject(r.Context(), db.UpsertCasObjectParams{
			OrgID: params.orgID, Digest: strings.TrimSpace(request.ArtifactDigest),
			SizeBytes: request.ArtifactSizeBytes,
			MediaType: strings.TrimSpace(request.ArtifactMediaType),
		}); err != nil {
			return err
		}
		artifact, err := work.q.CreateArtifact(r.Context(), db.CreateArtifactParams{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), OrgID: params.orgID,
			ProjectID: params.mount.ProjectID, EnvironmentID: params.mount.EnvironmentID,
			Digest: strings.TrimSpace(request.ArtifactDigest),
			Kind:   db.ArtifactKindWorkspaceVersion, SizeBytes: request.ArtifactSizeBytes,
			MediaType:                 strings.TrimSpace(request.ArtifactMediaType),
			CreatedByWorkerInstanceID: params.workerID,
		})
		if err != nil {
			return err
		}
		var publicID string
		staged, err := createWithPublicID(
			r.Context(),
			[]publicIDSlot{{prefix: publicid.WorkspaceVersion, value: &publicID}},
			func() (db.StageWorkspaceExecCaptureRow, error) {
				return work.q.StageWorkspaceExecCapture(
					r.Context(),
					db.StageWorkspaceExecCaptureParams{
						WorkspaceMountID:         params.mount.ID,
						WorkerInstanceID:         params.workerID,
						WorkerEpoch:              params.epoch,
						WorkspaceVersionID:       pgvalue.UUID(uuid.Must(uuid.NewV7())),
						WorkspaceVersionPublicID: publicID,
						ArtifactID:               artifact.ID,
						ContentDigest:            strings.TrimSpace(request.ArtifactDigest),
						SizeBytes:                request.ArtifactSizeBytes,
						EntryCount:               request.ArtifactEntryCount,
					},
				)
			},
		)
		if err != nil {
			return err
		}
		versionID = staged.ID
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("Workspace capture is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("stage Workspace capture"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerWorkspaceMountCaptureResponse{
		VersionID: pgvalue.MustUUIDValue(versionID).String(),
	})
}

func (s *Server) workerStopWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMountStopRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Workspace stop JSON: %w", err)))
		return
	}
	params, err := s.workspaceMountTransition(r.Context(), request.OrgID, request.WorkspaceMountID)
	if err != nil {
		writeError(w, err)
		return
	}
	var stopped db.WorkspaceMount
	err = s.inTx(r.Context(), func(work *txWork) error {
		locator, locatorErr := work.q.GetWorkspaceExecLocatorForMount(
			r.Context(),
			db.GetWorkspaceExecLocatorForMountParams{
				OrgID: params.orgID, WorkspaceMountID: params.mount.ID,
			},
		)
		if locatorErr == nil {
			authority, err := work.q.LockWorkspaceExecWorkerAuthority(
				r.Context(),
				db.LockWorkspaceExecWorkerAuthorityParams{
					OrgID: params.orgID, ProcessID: locator.ID,
					WorkspaceMountID: params.mount.ID,
					WorkerInstanceID: params.workerID, WorkerEpoch: params.epoch,
				},
			)
			if err != nil {
				return err
			}
			if err := s.finalizeWorkspaceExec(r.Context(), work, authority); err != nil {
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
		})
		if err != nil {
			return err
		}
		stopped = workspaceMountFromStop(row)
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("Workspace stop is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("stop Workspace mount"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceMountResponse(stopped))
}

func (s *Server) finalizeWorkspaceExec(
	ctx context.Context,
	work *txWork,
	authority db.LockWorkspaceExecWorkerAuthorityRow,
) error {
	mount := authority.WorkspaceMount
	process := authority.WorkspaceProcess
	lease := authority.WorkspaceLease
	var versionID pgtype.UUID
	finalState := db.WorkspaceProcessStateFailed
	if mount.FinalizationKind.String == "capture" {
		if !mount.StagedVersionID.Valid {
			return errors.New("Workspace exec capture is not staged")
		}
		if _, err := work.q.CommitStagedWorkspaceExecVersion(
			ctx,
			db.CommitStagedWorkspaceExecVersionParams{
				VersionID:   mount.StagedVersionID,
				WorkspaceID: mount.WorkspaceID,
			},
		); err != nil {
			return err
		}
		versionID = mount.StagedVersionID
		finalState = db.WorkspaceProcessStateExited
	}
	if _, err := work.q.FinalizeWorkspaceExecWorkspace(
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
		return err
	}
	finalized, err := work.q.FinalizeWorkspaceExecProcess(
		ctx,
		db.FinalizeWorkspaceExecProcessParams{
			State:            finalState,
			ReasonCode:       mount.FinalizationReasonCode,
			Error:            mount.FinalizationError,
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
	receipt, err := json.Marshal(map[string]string{
		"process_id":  pgvalue.MustUUIDValue(finalized.ID).String(),
		"reason_code": mount.FinalizationReasonCode.String,
	})
	if err != nil {
		return err
	}
	claims, err := s.claims.TransactionForQueries(work.q)
	if err != nil {
		return err
	}
	if finalState == db.WorkspaceProcessStateExited {
		_, err = claims.Complete(ctx, claim, receipt)
	} else {
		_, err = claims.Fail(ctx, claim, receipt)
	}
	return err
}

func (s *Server) workerFailWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMountFailRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Workspace mount failure JSON: %w", err)))
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
		failed = row
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
		writeError(w, conflict(errors.New("Workspace mount is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("fail Workspace mount"))
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
	if mount.StagedVersionID.Valid {
		affected, err := work.q.DiscardStagedWorkspaceExecVersion(
			ctx,
			db.DiscardStagedWorkspaceExecVersionParams{
				VersionID:   mount.StagedVersionID,
				WorkspaceID: process.WorkspaceID,
			},
		)
		if err != nil {
			return err
		}
		if affected != 1 {
			return errors.New("staged Workspace exec version is not discardable")
		}
	}
	if _, err := work.q.MarkWorkspaceExecRecoveryRequired(
		ctx,
		db.MarkWorkspaceExecRecoveryRequiredParams{
			WorkspaceID:         process.WorkspaceID,
			BaseVersionID:       process.BaseVersionID,
			OwnershipGeneration: lease.OwnershipGeneration,
			WriterGeneration:    lease.WriterGeneration,
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
	receipt, err := json.Marshal(map[string]string{
		"process_id":  pgvalue.MustUUIDValue(failed.ID).String(),
		"reason_code": reasonCode,
	})
	if err != nil {
		return err
	}
	claims, err := s.claims.TransactionForQueries(work.q)
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
	mount, err := s.db.GetWorkspaceMountForWorkerTransition(
		ctx,
		db.GetWorkspaceMountForWorkerTransitionParams{
			OrgID: orgID, ID: mountID,
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:      worker.WorkerEpoch,
		},
	)
	if err != nil {
		return workspaceMountTransitionAuthority{}, err
	}
	return workspaceMountTransitionAuthority{
		orgID: orgID, workerID: pgvalue.UUID(worker.WorkerInstanceID),
		epoch: worker.WorkerEpoch, mount: mount,
	}, nil
}

func parseWorkspaceWorkerIDs(rawOrgID, rawMountID string) (pgtype.UUID, pgtype.UUID, error) {
	orgID, err := uuid.Parse(strings.TrimSpace(rawOrgID))
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, badRequest(errors.New("org_id must be a UUID"))
	}
	mountID, err := uuid.Parse(strings.TrimSpace(rawMountID))
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, badRequest(errors.New("workspace_mount_id must be a UUID"))
	}
	return pgvalue.UUID(orgID), pgvalue.UUID(mountID), nil
}

func guestChannelTokenHash(value string) string {
	return sha256sum.HexBytes([]byte(strings.TrimSpace(value)))
}

func workspaceMountResponse(row db.WorkspaceMount) api.WorkspaceMountResponse {
	response := api.WorkspaceMountResponse{
		ID:               pgvalue.MustUUIDValue(row.ID).String(),
		ProjectID:        pgvalue.MustUUIDValue(row.ProjectID).String(),
		EnvironmentID:    pgvalue.MustUUIDValue(row.EnvironmentID).String(),
		WorkspaceID:      pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		BaseVersionID:    pgvalue.MustUUIDValue(row.MaterializedVersionID).String(),
		WorkerInstanceID: pgvalue.MustUUIDValue(row.WorkerInstanceID).String(),
		State:            string(row.State), ClaimAttempt: row.ClaimAttempt,
		FencingGeneration:    row.FencingGeneration,
		DirtyGeneration:      row.DirtyGeneration,
		FinalizationKind:     row.FinalizationKind.String,
		ReservationExpiresAt: pgTime(row.GuestChannelTokenExpiresAt),
		LastHeartbeatAt:      pgTime(row.UpdatedAt),
		CreatedAt:            row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
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

func projectWorkerWorkspaceMount(row db.ClaimWorkspaceMountRow) (*api.WorkerWorkspaceMount, error) {
	var network compute.NetworkPolicy
	if err := json.Unmarshal(row.NetworkPolicy, &network); err != nil {
		return nil, err
	}
	if err := network.Validate(); err != nil {
		return nil, err
	}
	return &api.WorkerWorkspaceMount{
		ID:                     pgvalue.MustUUIDValue(row.ID).String(),
		OrgID:                  pgvalue.MustUUIDValue(row.OrgID).String(),
		ProjectID:              pgvalue.MustUUIDValue(row.ProjectID).String(),
		EnvironmentID:          pgvalue.MustUUIDValue(row.EnvironmentID).String(),
		WorkspaceID:            pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		DeploymentDefinitionID: pgvalue.MustUUIDValue(row.DeploymentDefinitionID).String(),
		BaseVersionID:          pgvalue.MustUUIDValue(row.MaterializedVersionID).String(),
		RuntimeInstanceID:      pgvalue.MustUUIDValue(row.RuntimeInstanceID).String(),
		NetworkSlotID:          pgvalue.MustUUIDValue(row.NetworkSlotID).String(),
		NetworkSlotGeneration:  row.NetworkSlotGeneration,
		RuntimeEpoch:           row.WorkerEpoch,
		GuestdChannelTokenHash: row.GuestChannelTokenHash,
		State:                  string(row.State), RuntimeID: row.RuntimeID,
		WorkspaceImage: api.CASObject{
			Digest: row.ImageArtifactDigest, SizeBytes: row.ImageArtifactSizeBytes,
			MediaType: row.ImageArtifactMediaType,
		},
		RootfsDigest: row.RootfsDigest,
		WorkspaceArtifact: api.WorkerWorkspaceArtifact{
			Digest:     row.WorkspaceArtifactDigest,
			MediaType:  row.WorkspaceArtifactMediaType,
			Encoding:   workspace.ArtifactEncoding,
			SizeBytes:  row.WorkspaceArtifactSizeBytes,
			EntryCount: row.WorkspaceEntryCount,
		},
		WorkspaceMountPath:      "/workspace",
		RequestedMilliCPU:       row.ReservedCpuMillis,
		RequestedMemoryMiB:      row.ReservedMemoryBytes / (1024 * 1024),
		RequestedDiskMiB:        row.ReservedWorkloadDiskBytes / (1024 * 1024),
		RequestedExecutionSlots: row.ReservedExecutionSlots,
		RuntimeABI:              row.RuntimeABI, Network: network,
		FencingGeneration: row.FencingGeneration,
		ExpiresAt:         row.GuestChannelTokenExpiresAt.Time,
	}, nil
}

func workspaceMountFromStop(row db.StopWorkspaceMountRow) db.WorkspaceMount {
	return db.WorkspaceMount{
		ID: row.ID, OrgID: row.OrgID, WorkerGroupID: row.WorkerGroupID,
		ProjectID: row.ProjectID, EnvironmentID: row.EnvironmentID,
		RegionID: row.RegionID, WorkerInstanceID: row.WorkerInstanceID,
		WorkerEpoch: row.WorkerEpoch, WorkspaceID: row.WorkspaceID,
		MaterializedVersionID: row.MaterializedVersionID,
		RuntimeInstanceID:     row.RuntimeInstanceID, ClaimAttempt: row.ClaimAttempt,
		GuestChannelTokenHash:      row.GuestChannelTokenHash,
		GuestChannelTokenExpiresAt: row.GuestChannelTokenExpiresAt,
		State:                      row.State, Request: row.Request, DirtyGeneration: row.DirtyGeneration,
		FencingGeneration: row.FencingGeneration, FinalizationKind: row.FinalizationKind,
		FinalizationReasonCode: row.FinalizationReasonCode,
		FinalizationError:      row.FinalizationError, StagedVersionID: row.StagedVersionID,
		RequestedAt: row.RequestedAt, MountedAt: row.MountedAt,
		UnmountedAt: row.UnmountedAt, StoppedAt: row.StoppedAt,
		LostAt: row.LostAt, FailedAt: row.FailedAt, TerminalAt: row.TerminalAt,
		TerminalReasonCode: row.TerminalReasonCode, TerminalError: row.TerminalError,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}
