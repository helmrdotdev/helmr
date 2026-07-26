package control

import (
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerClaimWorkspaceExec(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceExecClaimRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Workspace exec claim JSON: %w", err)))
		return
	}
	orgID, mountID, err := parseWorkspaceWorkerIDs(request.OrgID, request.WorkspaceMountID)
	if err != nil {
		writeError(w, err)
		return
	}
	worker := workerFromContext(r.Context())
	var (
		authority  db.LockWorkspaceExecWorkerAuthorityRow
		envelopes  []secret.DeliveryEnvelope
		stdin      []byte
		capability string
	)
	err = s.inTx(r.Context(), func(work *txWork) error {
		locator, err := work.q.GetWorkspaceExecLocatorForMount(
			r.Context(),
			db.GetWorkspaceExecLocatorForMountParams{
				OrgID:            orgID,
				WorkspaceMountID: mountID,
			},
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("locate Workspace exec: %w", err)
		}
		envelopes, err = secret.LockProcessDelivery(
			r.Context(),
			work.q,
			locator.ID,
			locator.WorkspaceID,
		)
		if err != nil {
			return err
		}
		authority, err = work.q.LockWorkspaceExecWorkerAuthority(
			r.Context(),
			db.LockWorkspaceExecWorkerAuthorityParams{
				OrgID:            orgID,
				ProcessID:        locator.ID,
				WorkspaceMountID: mountID,
				WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
				WorkerEpoch:      worker.WorkerEpoch,
			},
		)
		if err != nil {
			return err
		}
		if authority.WorkspaceProcess.State == db.WorkspaceProcessStateExitRequested {
			return nil
		}
		if authority.WorkspaceProcess.State == db.WorkspaceProcessStateStarting {
			started, err := work.q.StartWorkspaceExec(r.Context(), db.StartWorkspaceExecParams{
				ProcessID:        authority.WorkspaceProcess.ID,
				WorkspaceMountID: authority.WorkspaceMount.ID,
			})
			if err != nil {
				return err
			}
			authority.WorkspaceProcess = started
		}
		derived, err := deriveWorkspaceCapability(
			s.workspaceFencingKeys,
			authority.WorkspaceLease,
		)
		if err != nil {
			return err
		}
		capability = derived.Token
		stdin = bytes.Clone(authority.WorkspaceProcess.Stdin)
		if len(stdin) > workspaceExecStdinMaxBytes {
			return errors.New("Workspace exec stdin exceeds its persisted limit")
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("Workspace exec claim is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("claim Workspace exec"))
		return
	}
	defer clearWorkspaceExecBytes(stdin)
	if !authority.WorkspaceProcess.ID.Valid ||
		authority.WorkspaceProcess.State == db.WorkspaceProcessStateExitRequested {
		writeJSON(w, http.StatusOK, api.WorkerWorkspaceExecClaimResponse{})
		return
	}
	environmentID, err := pgvalue.UUIDValue(authority.WorkspaceProcess.EnvironmentID)
	if err != nil {
		writeError(w, errors.New("Workspace exec environment is invalid"))
		return
	}
	materials, err := s.secretDelivery.OpenDeliveries(environmentID, envelopes)
	if err != nil {
		writeError(w, errors.New("open Workspace exec Secrets"))
		return
	}
	defer clearWorkspaceExecMaterials(materials)
	deliveries, err := projectSecretDeliveries(materials)
	if err != nil {
		writeError(w, errors.New("project Workspace exec Secrets"))
		return
	}
	defer clearWorkspaceSecretDeliveries(deliveries)
	writeJSON(w, http.StatusOK, api.WorkerWorkspaceExecClaimResponse{
		Exec: &api.WorkerWorkspaceExec{
			ProcessID:           pgvalue.MustUUIDValue(authority.WorkspaceProcess.ID).String(),
			WorkspaceID:         pgvalue.MustUUIDValue(authority.WorkspaceProcess.WorkspaceID).String(),
			WorkspaceMountID:    pgvalue.MustUUIDValue(authority.WorkspaceMount.ID).String(),
			RequestFingerprint:  hex.EncodeToString(authority.RequestFingerprint),
			Request:             bytes.Clone(authority.WorkspaceProcess.Request),
			Stdin:               stdin,
			Secrets:             deliveries,
			WorkspaceLeaseID:    pgvalue.MustUUIDValue(authority.WorkspaceLease.ID).String(),
			WriteCapability:     capability,
			FencingGeneration:   authority.WorkspaceLease.MountFencingGeneration,
			OwnershipGeneration: authority.WorkspaceLease.OwnershipGeneration,
			WriterGeneration:    authority.WorkspaceLease.WriterGeneration,
			ExpiresAt:           authority.WorkspaceLease.ExpiresAt.Time,
		},
	})
}

func clearWorkspaceSecretDeliveries(deliveries []api.WorkerSecretDelivery) {
	for index := range deliveries {
		clearWorkspaceExecBytes(deliveries[index].Value)
	}
}

func clearWorkspaceExecMaterials(materials []secret.DeliveryMaterial) {
	for index := range materials {
		clearWorkspaceExecBytes(materials[index].Value)
	}
}

func clearWorkspaceExecBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func (s *Server) workerCompleteWorkspaceExec(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceExecCompleteRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Workspace exec completion JSON: %w", err)))
		return
	}
	orgID, err := uuid.Parse(strings.TrimSpace(request.OrgID))
	if err != nil {
		writeError(w, badRequest(errors.New("org_id must be a UUID")))
		return
	}
	processID, err := uuid.Parse(strings.TrimSpace(request.ProcessID))
	if err != nil {
		writeError(w, badRequest(errors.New("process_id must be a UUID")))
		return
	}
	leaseID, err := uuid.Parse(strings.TrimSpace(request.WorkspaceLeaseID))
	if err != nil {
		writeError(w, badRequest(errors.New("workspace_lease_id must be a UUID")))
		return
	}
	if len(request.Stdout) > workspaceExecOutputMaxBytes ||
		len(request.Stderr) > workspaceExecOutputMaxBytes {
		writeError(w, badRequest(errors.New("Workspace exec output exceeds its limit")))
		return
	}
	finalizationKind, reasonCode, resultError, err := normalizeWorkspaceExecOutcome(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	var mount db.WorkspaceMount
	err = s.inTx(r.Context(), func(work *txWork) error {
		locator, err := work.q.GetWorkspaceExecLocator(
			r.Context(),
			db.GetWorkspaceExecLocatorParams{
				OrgID:     pgvalue.UUID(orgID),
				ProcessID: pgvalue.UUID(processID),
			},
		)
		if err != nil {
			return err
		}
		authority, err := work.q.LockWorkspaceExecWorkerAuthority(
			r.Context(),
			db.LockWorkspaceExecWorkerAuthorityParams{
				OrgID:            pgvalue.UUID(orgID),
				ProcessID:        pgvalue.UUID(processID),
				WorkspaceMountID: locator.WorkspaceMountID,
				WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
				WorkerEpoch:      worker.WorkerEpoch,
			},
		)
		if err != nil {
			return err
		}
		if authority.WorkspaceLease.ID != pgvalue.UUID(leaseID) ||
			authority.WorkspaceLease.MountFencingGeneration != request.FencingGeneration ||
			authority.WorkspaceLease.OwnershipGeneration != request.OwnershipGeneration ||
			authority.WorkspaceLease.WriterGeneration != request.WriterGeneration ||
			hex.EncodeToString(authority.RequestFingerprint) != strings.TrimSpace(request.RequestFingerprint) {
			return errors.New("Workspace exec completion fence is stale")
		}
		capability, err := deriveWorkspaceCapability(s.workspaceFencingKeys, authority.WorkspaceLease)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare(
			[]byte(capability.Token),
			[]byte(strings.TrimSpace(request.WriteCapability)),
		) != 1 {
			return errors.New("Workspace exec write capability is invalid")
		}
		var exitCode pgtype.Int4
		if request.ExitCode != nil {
			exitCode = pgtype.Int4{Int32: *request.ExitCode, Valid: true}
		}
		if _, err := work.q.SetWorkspaceExecResult(
			r.Context(),
			db.SetWorkspaceExecResultParams{
				ExitCode:         exitCode,
				Stdout:           nonNilWorkspaceExecBytes(request.Stdout),
				Stderr:           nonNilWorkspaceExecBytes(request.Stderr),
				ProcessID:        authority.WorkspaceProcess.ID,
				WorkspaceMountID: authority.WorkspaceMount.ID,
			},
		); err != nil {
			return err
		}
		requested, err := work.q.RequestWorkspaceExecMountFinalization(
			r.Context(),
			db.RequestWorkspaceExecMountFinalizationParams{
				FinalizationKind: pgvalue.Text(finalizationKind),
				ReasonCode:       pgvalue.Text(reasonCode),
				Error:            resultError,
				WorkspaceMountID: authority.WorkspaceMount.ID,
				WorkerInstanceID: authority.WorkspaceMount.WorkerInstanceID,
				WorkerEpoch:      authority.WorkspaceMount.WorkerEpoch,
			},
		)
		if err != nil {
			return err
		}
		mount = workspaceMountFromFinalization(requested)
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("Workspace exec completion is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("complete Workspace exec"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceMountResponse(mount))
}

func normalizeWorkspaceExecOutcome(
	request api.WorkerWorkspaceExecCompleteRequest,
) (string, string, []byte, error) {
	outcome := strings.TrimSpace(request.Outcome)
	if outcome == "exited" {
		if request.ExitCode == nil || len(request.Error) != 0 {
			return "", "", nil, errors.New("exited Workspace exec requires exit_code and no error")
		}
		return "capture", "workspace_exec_completed", nil, nil
	}
	switch outcome {
	case "workspace_exec_failed",
		"workspace_exec_secret_delivery_failed",
		"workspace_exec_timed_out",
		"workspace_exec_signaled",
		"workspace_exec_launch_failed",
		"workspace_exec_output_limit_exceeded",
		"workspace_exec_result_uncertain":
	default:
		return "", "", nil, fmt.Errorf("Workspace exec outcome %q is unsupported", outcome)
	}
	errorJSON, err := normalizedJSONObject(request.Error, "error")
	if err != nil {
		return "", "", nil, err
	}
	if len(errorJSON) == 0 {
		errorJSON, _ = json.Marshal(map[string]string{"code": outcome})
	}
	return "discard", outcome, errorJSON, nil
}

func workspaceMountFromFinalization(row db.RequestWorkspaceExecMountFinalizationRow) db.WorkspaceMount {
	return db.WorkspaceMount(row)
}
