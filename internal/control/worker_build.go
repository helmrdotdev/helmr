package control

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const deploymentBuildLeaseDuration = 30 * time.Minute

func (s *Server) workerLeaseDeploymentBuild(w http.ResponseWriter, r *http.Request) {
	worker := workerFromContext(r.Context())
	if s.db == nil {
		writeError(w, unavailable(errors.New("deployment build storage is not configured")))
		return
	}
	var request api.WorkerDeploymentBuildLeaseRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker deployment build lease JSON: %w", err)))
		return
	}
	leaseExpiresAt := time.Now().Add(deploymentBuildLeaseDuration)
	if s.buildPolicy == nil || s.managerStore == nil {
		writeError(w, unavailable(errors.New("deployment build authority is not configured")))
		return
	}
	var response api.WorkerDeploymentBuildLeaseResponse
	err := s.inTx(r.Context(), func(work *txWork) error {
		row, err := work.q.ClaimNextDeploymentBuildLease(r.Context(), db.ClaimNextDeploymentBuildLeaseParams{
			WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
			ExpiresAt: pgvalue.Timestamptz(leaseExpiresAt),
		})
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim assigned deployment build: %w", err)
		}
		runtimeDigest, err := deployment.RuntimeDigestString(row.BuildRuntimeDigest)
		if err != nil {
			return fmt.Errorf("read deployment build runtime digest: %w", err)
		}
		toolchainDigest, err := deployment.SHA256DigestString(
			row.BuildStandardToolchainDigest,
		)
		if err != nil {
			return fmt.Errorf("read deployment build standard toolchain digest: %w", err)
		}
		target, err := s.buildPolicy.Resolve(
			runtimeDigest,
			toolchainDigest,
			row.BuildContractVersion,
		)
		if err != nil {
			return fmt.Errorf("resolve deployment build target: %w", err)
		}
		runtimeTarget := target.Runtime
		if string(runtimeTarget.Architecture) != row.BuildArchitecture {
			return errors.New("deployment build runtime descriptor does not match persisted architecture")
		}
		runtimeWire, err := deployment.RuntimeDescriptorWire(runtimeTarget)
		if err != nil {
			return fmt.Errorf("encode deployment build runtime descriptor: %w", err)
		}
		deploymentID := pgvalue.MustUUIDValue(row.DeploymentID).String()
		lease := api.WorkerDeploymentBuildLease{
			ID:                         pgvalue.MustUUIDValue(row.ID).String(),
			OrgID:                      pgvalue.MustUUIDValue(row.OrgID).String(),
			ProjectID:                  pgvalue.MustUUIDValue(row.ProjectID).String(),
			EnvironmentID:              pgvalue.MustUUIDValue(row.EnvironmentID).String(),
			DeploymentID:               deploymentID,
			WorkerGroupID:              row.WorkerGroupID,
			WorkerInstanceID:           pgvalue.MustUUIDValue(row.WorkerInstanceID).String(),
			WorkerEpoch:                row.WorkerEpoch,
			LeaseSequence:              row.LeaseSequence,
			WorkerProtocolVersion:      row.WorkerProtocolVersion,
			ExpiresAt:                  leaseExpiresAt,
			RequestedWorkloadDiskBytes: row.RequestedWorkloadDiskBytes,
			RequestedScratchBytes:      row.RequestedScratchBytes,
			RequestedCPUMillis:         row.RequestedCpuMillis,
			RequestedMemoryBytes:       row.RequestedMemoryBytes,
			RequestedBuildExecutors:    row.RequestedBuildExecutors,
		}
		build := api.WorkerDeploymentBuild{
			ID:                    deploymentID,
			Version:               row.Version,
			APIVersion:            row.ApiVersion,
			SDKVersion:            row.SdkVersion,
			CLIVersion:            row.CliVersion,
			WorkerProtocolVersion: row.WorkerProtocolVersion,
			ProjectID:             pgvalue.MustUUIDValue(row.ProjectID).String(),
			EnvironmentID:         pgvalue.MustUUIDValue(row.EnvironmentID).String(),
			DeploymentSource: api.DeploymentSourceArtifact{
				Digest:    row.DeploymentSourceDigest,
				SizeBytes: row.SourceSizeBytes,
				MediaType: row.SourceMediaType,
			},
			Runtime:                 runtimeWire,
			StandardToolchainDigest: target.StandardToolchainDigest,
			BuildContractVersion:    target.BuildContractVersion,
		}
		response = api.WorkerDeploymentBuildLeaseResponse{Lease: &lease, Deployment: &build}
		return nil
	})
	if err != nil {
		s.log.Error("claim assigned deployment build failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("claim assigned deployment build"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerStartDeploymentBuild(w http.ResponseWriter, r *http.Request) {
	worker := workerFromContext(r.Context())
	if s.db == nil {
		writeError(w, unavailable(errors.New("deployment build storage is not configured")))
		return
	}
	var request api.WorkerDeploymentBuildStartRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker deployment build start JSON: %w", err)))
		return
	}
	lease := request.Lease
	orgID, _, _, deploymentID, err := parseDeploymentBuildLeaseIDs(lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	leaseID, err := uuid.Parse(strings.TrimSpace(lease.ID))
	if err != nil || lease.WorkerGroupID != worker.WorkerGroupID || lease.WorkerInstanceID != worker.WorkerInstanceID.String() || lease.WorkerEpoch != worker.WorkerEpoch || lease.LeaseSequence < 1 || lease.LeaseSequence > 3 || lease.WorkerProtocolVersion != worker.ProtocolVersion {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	}
	started, err := s.db.GetStartedDeploymentBuildLease(r.Context(), db.GetStartedDeploymentBuildLeaseParams{
		OrgID: orgID, DeploymentID: deploymentID, BuildLeaseID: pgvalue.UUID(leaseID),
		LeaseSequence: lease.LeaseSequence,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
		WorkerProtocolVersion:      worker.ProtocolVersion,
		RequestedWorkloadDiskBytes: lease.RequestedWorkloadDiskBytes, RequestedScratchBytes: lease.RequestedScratchBytes,
		RequestedCpuMillis: lease.RequestedCPUMillis, RequestedMemoryBytes: lease.RequestedMemoryBytes,
		RequestedBuildExecutors: lease.RequestedBuildExecutors,
	})
	if err == nil {
		lease.ExpiresAt = pgvalue.Time(started.ExpiresAt)
		writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildStartResponse{Lease: lease})
		return
	}
	if !isNoRows(err) {
		writeError(w, errors.New("get started deployment build"))
		return
	}
	expiresAt := time.Now().Add(deploymentBuildLeaseDuration)
	started, err = s.db.StartDeploymentBuildLease(r.Context(), db.StartDeploymentBuildLeaseParams{
		ExpiresAt: pgvalue.Timestamptz(expiresAt), OrgID: orgID, DeploymentID: deploymentID,
		BuildLeaseID: pgvalue.UUID(leaseID), LeaseSequence: lease.LeaseSequence,
		WorkerGroupID:    worker.WorkerGroupID,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
		RequestedWorkloadDiskBytes: lease.RequestedWorkloadDiskBytes,
		RequestedScratchBytes:      lease.RequestedScratchBytes,
		RequestedCpuMillis:         lease.RequestedCPUMillis,
		RequestedMemoryBytes:       lease.RequestedMemoryBytes,
		RequestedBuildExecutors:    lease.RequestedBuildExecutors,
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("start deployment build"))
		return
	}
	lease.ExpiresAt = pgvalue.Time(started.ExpiresAt)
	writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildStartResponse{Lease: lease})
}

func (s *Server) workerRenewDeploymentBuild(w http.ResponseWriter, r *http.Request) {
	worker := workerFromContext(r.Context())
	if s.db == nil {
		writeError(w, unavailable(errors.New("deployment build storage is not configured")))
		return
	}
	var request api.WorkerDeploymentBuildRenewRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker deployment build renew JSON: %w", err)))
		return
	}
	lease := request.Lease
	orgID, _, _, deploymentID, err := parseDeploymentBuildLeaseIDs(lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	leaseID, err := uuid.Parse(strings.TrimSpace(lease.ID))
	if err != nil || lease.WorkerGroupID != worker.WorkerGroupID || lease.WorkerInstanceID != worker.WorkerInstanceID.String() || lease.WorkerEpoch != worker.WorkerEpoch || lease.LeaseSequence < 1 || lease.LeaseSequence > 3 || lease.WorkerProtocolVersion != worker.ProtocolVersion {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	}
	expiresAt := time.Now().Add(deploymentBuildLeaseDuration)
	renewed, err := s.db.RenewDeploymentBuildLease(r.Context(), db.RenewDeploymentBuildLeaseParams{
		ExpiresAt: pgvalue.Timestamptz(expiresAt), OrgID: orgID, DeploymentID: deploymentID,
		BuildLeaseID: pgvalue.UUID(leaseID), LeaseSequence: lease.LeaseSequence,
		WorkerGroupID:    worker.WorkerGroupID,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("renew deployment build"))
		return
	}
	lease.ExpiresAt = pgvalue.Time(renewed.ExpiresAt)
	writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildRenewResponse{Lease: lease})
}

func (s *Server) workerRejectDeploymentBuild(w http.ResponseWriter, r *http.Request) {
	worker := workerFromContext(r.Context())
	if s.db == nil {
		writeError(w, unavailable(errors.New("deployment build storage is not configured")))
		return
	}
	var request api.WorkerDeploymentBuildRejectRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker deployment build reject JSON: %w", err)))
		return
	}
	lease := request.Lease
	orgID, projectID, environmentID, deploymentID, err := parseDeploymentBuildLeaseIDs(lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	leaseID, err := uuid.Parse(strings.TrimSpace(lease.ID))
	if err != nil || lease.WorkerGroupID != worker.WorkerGroupID || lease.WorkerInstanceID != worker.WorkerInstanceID.String() || lease.WorkerEpoch != worker.WorkerEpoch || lease.LeaseSequence < 1 || lease.LeaseSequence > 3 || lease.WorkerProtocolVersion != worker.ProtocolVersion {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	}
	reason := strings.TrimSpace(request.ReasonCode)
	if reason == "" {
		reason = "worker_preflight_rejected"
	}
	fingerprint, err := terminalRequestFingerprint("deployment_build.reject", struct {
		ReasonCode string          `json:"reason_code"`
		Error      json.RawMessage `json:"error,omitempty"`
	}{ReasonCode: reason, Error: request.Error})
	if err != nil {
		writeError(w, errors.New("fingerprint deployment build rejection"))
		return
	}
	terminal, err := s.db.GetDeploymentBuildTerminalResult(r.Context(), db.GetDeploymentBuildTerminalResultParams{
		OrgID: orgID, DeploymentID: deploymentID, BuildLeaseID: pgvalue.UUID(leaseID),
		LeaseSequence: lease.LeaseSequence,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
	})
	if err == nil {
		if terminal.State != db.DeploymentBuildLeaseStateRejected || !terminal.TerminalRequestFingerprint.Valid || terminal.TerminalRequestFingerprint.String != fingerprint {
			writeError(w, conflict(errors.New("deployment build lease already has a different terminal result")))
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !isNoRows(err) {
		writeError(w, errors.New("get terminal deployment build"))
		return
	}
	err = s.inTx(r.Context(), func(work *txWork) error {
		locked, err := work.q.LockDeploymentBuildTerminalFence(r.Context(), db.LockDeploymentBuildTerminalFenceParams{
			OrgID:                 orgID,
			ProjectID:             projectID,
			EnvironmentID:         environmentID,
			DeploymentID:          deploymentID,
			BuildLeaseID:          pgvalue.UUID(leaseID),
			LeaseSequence:         lease.LeaseSequence,
			WorkerGroupID:         worker.WorkerGroupID,
			WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:           worker.WorkerEpoch,
			WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if isNoRows(err) {
			return conflict(errors.New("deployment build lease is stale"))
		}
		if err != nil {
			return errors.New("lock deployment build terminal fence")
		}
		if locked.State == db.DeploymentBuildLeaseStateSucceeded || locked.State == db.DeploymentBuildLeaseStateFailed || locked.State == db.DeploymentBuildLeaseStateRejected {
			if locked.State != db.DeploymentBuildLeaseStateRejected ||
				!locked.TerminalRequestFingerprint.Valid ||
				locked.TerminalRequestFingerprint.String != fingerprint {
				return conflict(errors.New("deployment build lease already has a different terminal result"))
			}
			return nil
		}
		if (locked.State != db.DeploymentBuildLeaseStateAssigned && locked.State != db.DeploymentBuildLeaseStateStarting) ||
			locked.DeploymentStatus != db.DeploymentStatusBuilding ||
			!locked.CurrentBuildLeaseID.Valid ||
			locked.CurrentBuildLeaseID != pgvalue.UUID(leaseID) ||
			!locked.ExpiresAt.Valid ||
			!locked.ExpiresAt.Time.After(time.Now()) {
			return conflict(errors.New("deployment build lease is stale"))
		}
		_, err = work.q.RejectDeploymentBuildLease(r.Context(), db.RejectDeploymentBuildLeaseParams{
			ReasonCode: pgtype.Text{String: reason, Valid: true}, Error: request.Error, OrgID: orgID, DeploymentID: deploymentID,
			BuildLeaseID: pgvalue.UUID(leaseID), LeaseSequence: lease.LeaseSequence,
			WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
			TerminalRequestFingerprint: fingerprint,
		})
		if isNoRows(err) {
			return conflict(errors.New("deployment build lease is stale"))
		}
		if err != nil {
			return errors.New("reject deployment build")
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerDeploymentBuildDeliveryFailed(w http.ResponseWriter, r *http.Request) {
	worker := workerFromContext(r.Context())
	if s.db == nil {
		writeError(w, unavailable(errors.New("deployment build storage is not configured")))
		return
	}
	var request api.WorkerDeploymentBuildDeliveryFailureRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker deployment build delivery failure JSON: %w", err)))
		return
	}
	if request.ReasonCode != api.WorkerDeploymentBuildDeliveryBuildGuestFailed &&
		request.ReasonCode != api.WorkerDeploymentBuildDeliveryProgramVerifierFailed {
		writeError(w, badRequest(errors.New("deployment build delivery failure reasonCode is invalid")))
		return
	}
	lease := request.Lease
	orgID, projectID, environmentID, deploymentID, err := parseDeploymentBuildLeaseIDs(lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	leaseID, err := uuid.Parse(strings.TrimSpace(lease.ID))
	if err != nil ||
		lease.WorkerGroupID != worker.WorkerGroupID ||
		lease.WorkerInstanceID != worker.WorkerInstanceID.String() ||
		lease.WorkerEpoch != worker.WorkerEpoch ||
		lease.LeaseSequence < 1 ||
		lease.LeaseSequence > 3 ||
		lease.WorkerProtocolVersion != worker.ProtocolVersion {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	}
	row, err := s.db.FailDeploymentBuildDelivery(r.Context(), db.FailDeploymentBuildDeliveryParams{
		OrgID:                 orgID,
		ProjectID:             projectID,
		EnvironmentID:         environmentID,
		DeploymentID:          deploymentID,
		BuildLeaseID:          pgvalue.UUID(leaseID),
		LeaseSequence:         lease.LeaseSequence,
		WorkerGroupID:         worker.WorkerGroupID,
		WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:           worker.WorkerEpoch,
		WorkerProtocolVersion: worker.ProtocolVersion,
		ReasonCode:            pgtype.Text{String: string(request.ReasonCode), Valid: true},
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("fail deployment build delivery"))
		return
	}
	if row.State != db.DeploymentBuildLeaseStateLost ||
		!row.TerminalReasonCode.Valid ||
		row.TerminalReasonCode.String != string(request.ReasonCode) ||
		!row.TerminalAt.Valid ||
		row.LeaseSequence != lease.LeaseSequence ||
		row.DeploymentID != deploymentID ||
		(row.DeploymentStatus != db.DeploymentStatusBuilding &&
			row.DeploymentStatus != db.DeploymentStatusDeployed &&
			row.DeploymentStatus != db.DeploymentStatusFailed) {
		writeError(w, errors.New("invalid deployment build delivery failure result"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildResponse{
		DeploymentID: lease.DeploymentID,
		Status:       string(row.DeploymentStatus),
	})
}

func (s *Server) workerCompleteDeploymentBuild(w http.ResponseWriter, r *http.Request) {
	worker := workerFromContext(r.Context())
	if s.db == nil {
		writeError(w, unavailable(errors.New("deployment build storage is not configured")))
		return
	}
	var request struct {
		Lease  api.WorkerDeploymentBuildLease `json:"lease"`
		Result json.RawMessage                `json:"result"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker deployment build completion JSON: %w", err)))
		return
	}
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	}
	orgID, projectID, environmentID, deploymentID, err := parseDeploymentBuildLeaseIDs(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	buildLeaseUUID, err := uuid.Parse(strings.TrimSpace(request.Lease.ID))
	if err != nil ||
		request.Lease.WorkerGroupID != worker.WorkerGroupID ||
		request.Lease.WorkerEpoch != worker.WorkerEpoch ||
		request.Lease.LeaseSequence < 1 ||
		request.Lease.LeaseSequence > 3 ||
		request.Lease.WorkerProtocolVersion != worker.ProtocolVersion {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	}
	result, resultErr := deployment.ParseBuildResult(request.Result)
	fingerprint := deploymentBuildResultFingerprint(request.Result)
	terminal, err := s.db.GetDeploymentBuildTerminalResult(r.Context(), db.GetDeploymentBuildTerminalResultParams{
		OrgID: orgID, DeploymentID: deploymentID, BuildLeaseID: pgvalue.UUID(buildLeaseUUID),
		LeaseSequence: request.Lease.LeaseSequence,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
	})
	if err == nil {
		if !terminal.TerminalRequestFingerprint.Valid || terminal.TerminalRequestFingerprint.String != fingerprint {
			writeError(w, conflict(errors.New("deployment build lease already has a different terminal result")))
			return
		}
		var status string
		switch terminal.State {
		case db.DeploymentBuildLeaseStateSucceeded:
			status = string(db.DeploymentStatusDeployed)
		case db.DeploymentBuildLeaseStateFailed:
			status = string(db.DeploymentStatusFailed)
		default:
			writeError(w, conflict(errors.New("deployment build lease was terminated by another operation")))
			return
		}
		writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildResponse{DeploymentID: request.Lease.DeploymentID, Status: status})
		return
	}
	if !isNoRows(err) {
		writeError(w, errors.New("get terminal deployment build"))
		return
	}
	buildWorkerInstanceID := pgvalue.UUID(worker.WorkerInstanceID)
	var response api.WorkerDeploymentBuildResponse
	err = s.inTx(r.Context(), func(work *txWork) error {
		workerState, err := work.q.LockDeploymentBuildWorkerCertification(
			r.Context(),
			db.LockDeploymentBuildWorkerCertificationParams{
				WorkerGroupID:         worker.WorkerGroupID,
				WorkerInstanceID:      buildWorkerInstanceID,
				WorkerEpoch:           worker.WorkerEpoch,
				WorkerProtocolVersion: worker.ProtocolVersion,
			},
		)
		if isNoRows(err) {
			return conflict(errors.New("deployment build worker certification was withdrawn"))
		}
		if err != nil {
			return errors.New("lock deployment build worker certification")
		}
		locked, err := work.q.LockDeploymentBuildTerminalFence(r.Context(), db.LockDeploymentBuildTerminalFenceParams{
			OrgID:                 orgID,
			ProjectID:             projectID,
			EnvironmentID:         environmentID,
			DeploymentID:          deploymentID,
			BuildLeaseID:          pgvalue.UUID(buildLeaseUUID),
			LeaseSequence:         request.Lease.LeaseSequence,
			WorkerGroupID:         worker.WorkerGroupID,
			WorkerInstanceID:      buildWorkerInstanceID,
			WorkerEpoch:           worker.WorkerEpoch,
			WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if isNoRows(err) {
			return conflict(errors.New("deployment build lease is stale"))
		}
		if err != nil {
			return errors.New("lock deployment build terminal fence")
		}
		if locked.State == db.DeploymentBuildLeaseStateSucceeded || locked.State == db.DeploymentBuildLeaseStateFailed || locked.State == db.DeploymentBuildLeaseStateRejected {
			if !locked.TerminalRequestFingerprint.Valid || locked.TerminalRequestFingerprint.String != fingerprint {
				return conflict(errors.New("deployment build lease already has a different terminal result"))
			}
			switch locked.State {
			case db.DeploymentBuildLeaseStateSucceeded:
				response = api.WorkerDeploymentBuildResponse{DeploymentID: request.Lease.DeploymentID, Status: string(db.DeploymentStatusDeployed)}
				return nil
			case db.DeploymentBuildLeaseStateFailed:
				response = api.WorkerDeploymentBuildResponse{DeploymentID: request.Lease.DeploymentID, Status: string(db.DeploymentStatusFailed)}
				return nil
			default:
				return conflict(errors.New("deployment build lease was terminated by another operation"))
			}
		}
		if locked.State != db.DeploymentBuildLeaseStateRunning ||
			locked.DeploymentStatus != db.DeploymentStatusBuilding ||
			!locked.CurrentBuildLeaseID.Valid ||
			locked.CurrentBuildLeaseID != pgvalue.UUID(buildLeaseUUID) ||
			!locked.ExpiresAt.Valid ||
			!locked.ExpiresAt.Time.After(time.Now()) {
			return conflict(errors.New("deployment build lease is stale"))
		}
		failBuildWithReason := func(reasonCode, fallback, message string) error {
			message = strings.TrimSpace(message)
			if message == "" {
				message = fallback
			}
			payload, err := boundedWorkerMessagePayload(message, fallback)
			if err != nil {
				return errors.New("marshal deployment build error")
			}
			if resultErr == nil && result.Logs != nil {
				if err := appendDeploymentBuildLogs(
					r.Context(),
					work.q,
					locked.OrgID,
					locked.ProjectID,
					locked.EnvironmentID,
					locked.DeploymentID,
					*result.Logs,
				); err != nil {
					return errors.New("record deployment build logs")
				}
			}
			row, err := work.q.FailDeploymentBuild(r.Context(), db.FailDeploymentBuildParams{
				Failure: payload, ReasonCode: pgtype.Text{String: reasonCode, Valid: true},
				TerminalRequestFingerprint: fingerprint,
				OrgID:                      orgID, ID: deploymentID, BuildLeaseID: pgvalue.UUID(buildLeaseUUID),
				BuildWorkerInstanceID: buildWorkerInstanceID,
				WorkerEpoch:           worker.WorkerEpoch,
				LeaseSequence:         request.Lease.LeaseSequence,
			})
			if isNoRows(err) {
				return conflict(errors.New("deployment build lease is stale"))
			}
			if err != nil {
				return errors.New("mark deployment build failed")
			}
			if err := appendDeploymentLifecycleEvent(r.Context(), work.q, row.OrgID, row.ProjectID, row.EnvironmentID, row.ID, "deployment.failed", "error", "worker", "failed", message); err != nil {
				return errors.New("record deployment event")
			}
			response = api.WorkerDeploymentBuildResponse{DeploymentID: pgvalue.MustUUIDValue(row.ID).String(), Status: string(row.Status)}
			return nil
		}
		failInvalid := func(message string) error {
			return failBuildWithReason(
				"output_invalid",
				"deployment build output is invalid",
				message,
			)
		}
		if resultErr != nil {
			return failInvalid(
				fmt.Sprintf("invalid deployment build result: %v", resultErr),
			)
		}
		if result.Outcome == deployment.BuildOutcomeFailed {
			return failBuildWithReason(
				string(result.Failed.Error.ReasonCode),
				"deployment build failed",
				result.Failed.Error.Message,
			)
		}
		if s.cas == nil {
			return errors.New("deployment build CAS is not configured")
		}
		if s.buildPolicy == nil {
			return errors.New("build policy is not configured")
		}
		runtimeDigest, err := deployment.RuntimeDigestString(locked.BuildRuntimeDigest)
		if err != nil {
			return errors.New("deployment build runtime digest is invalid")
		}
		toolchainDigest, err := deployment.SHA256DigestString(
			locked.BuildStandardToolchainDigest,
		)
		if err != nil {
			return errors.New("deployment build standard toolchain digest is invalid")
		}
		target, err := s.buildPolicy.Resolve(
			runtimeDigest,
			toolchainDigest,
			locked.BuildContractVersion,
		)
		if err != nil || string(target.Runtime.Architecture) != locked.BuildArchitecture {
			return errors.New("deployment build target is not registered")
		}
		if err := deployment.ValidateBuildResultTarget(
			result,
			target.Runtime.Digest,
			target.Runtime.Architecture,
		); err != nil {
			return failInvalid(err.Error())
		}
		succeeded := *result.Succeeded
		source, err := s.cas.Get(r.Context(), locked.DeploymentSourceDigest)
		if err != nil {
			return fmt.Errorf("read deployment source authority: %w", err)
		}
		selection, inspectErr := deployment.InspectSource(source)
		closeErr := source.Close()
		if inspectErr != nil {
			return failInvalid(inspectErr.Error())
		}
		if closeErr != nil {
			return fmt.Errorf("close deployment source authority: %w", closeErr)
		}
		if err := validateManagerAuthority(
			r.Context(),
			s.managerStore,
			locked.DeploymentSourceDigest,
			selection,
			target,
			succeeded.Provenance,
		); err != nil {
			if errors.Is(err, errManagerAuthorityMismatch) {
				return failInvalid(err.Error())
			}
			return fmt.Errorf("validate package manager authority: %w", err)
		}
		objects, err := deploymentBuildObjects(succeeded)
		if err != nil {
			return failInvalid(err.Error())
		}
		if err := s.verifyDeploymentBuildArtifacts(r.Context(), objects); err != nil {
			var mismatch *deploymentBuildArtifactMismatch
			if errors.As(err, &mismatch) {
				return failInvalid(err.Error())
			}
			return fmt.Errorf("verify deployment build artifacts: %w", err)
		}
		if !workerState.RuntimeArch.Valid ||
			workerState.RuntimeArch.String != locked.BuildArchitecture {
			return conflict(errors.New("deployment build worker certification was withdrawn"))
		}

		workspaceImages := make(map[string]deployment.WorkspaceImageArtifact)
		for _, image := range succeeded.WorkspaceImages {
			workspaceImages[image.DeclaredID] = image.Artifact
		}
		type normalizedDefinition struct {
			input          deployment.DefinitionInput
			manifest       []byte
			manifestDigest [sha256.Size]byte
		}
		definitions := make([]normalizedDefinition, 0, len(succeeded.Plan.Definitions))
		for _, definition := range succeeded.Plan.Definitions {
			var workspaceImage *deployment.WorkspaceImageArtifact
			if definition.Workspace != nil {
				image, ok := workspaceImages[definition.DeclaredID]
				if !ok {
					return failInvalid(
						fmt.Sprintf(
							"Workspace %q has no image Artifact",
							definition.DeclaredID,
						),
					)
				}
				workspaceImage = &image
			}
			manifest, manifestDigest, err := deploymentDefinitionManifest(
				definition,
				workspaceImage,
			)
			if err != nil {
				return failInvalid(err.Error())
			}
			definitions = append(definitions, normalizedDefinition{
				input:          definition,
				manifest:       manifest,
				manifestDigest: manifestDigest,
			})
		}
		queueConfig, err := deployment.QueueConfigFromPlan(succeeded.Plan)
		if err != nil {
			return failInvalid(err.Error())
		}
		queueConfigJSON, err := deployment.CanonicalQueueConfig(queueConfig)
		if err != nil {
			return failInvalid(err.Error())
		}

		for _, object := range objects {
			if _, err := work.q.UpsertCasObject(r.Context(), db.UpsertCasObjectParams{
				OrgID:     orgID,
				Digest:    object.Digest,
				SizeBytes: object.SizeBytes,
				MediaType: object.MediaType,
			}); err != nil {
				return fmt.Errorf("record deployment build CAS object: %w", err)
			}
		}

		artifactByRole := make(map[string]db.Artifact)
		createArtifact := func(
			kind db.ArtifactKind,
			object deploymentBuildObject,
		) (db.Artifact, error) {
			key := string(kind) + "\x00" + object.Digest
			if artifact, ok := artifactByRole[key]; ok {
				return artifact, nil
			}
			artifact, err := createDeploymentBuildArtifact(
				r.Context(),
				work.q,
				orgID,
				projectID,
				environmentID,
				buildWorkerInstanceID,
				kind,
				object,
			)
			if err != nil {
				return db.Artifact{}, err
			}
			artifactByRole[key] = artifact
			return artifact, nil
		}

		var programCodeArtifactID pgtype.UUID
		var programDependencyArtifactID pgtype.UUID
		var programRuntimeDigest []byte
		var programArchitecture pgtype.Text
		if succeeded.ProgramReceipt != nil {
			codeArtifact, err := createArtifact(
				db.ArtifactKindDeploymentProgramCode,
				deploymentBuildObjectFromProgram(succeeded.ProgramReceipt.Code),
			)
			if err != nil {
				return fmt.Errorf("record deployment program code: %w", err)
			}
			dependencyArtifact, err := createArtifact(
				db.ArtifactKindDeploymentProgramDependencies,
				deploymentBuildObjectFromProgram(succeeded.ProgramReceipt.Dependencies),
			)
			if err != nil {
				return fmt.Errorf("record deployment program dependencies: %w", err)
			}
			programCodeArtifactID = codeArtifact.ID
			programDependencyArtifactID = dependencyArtifact.ID
			programRuntimeDigest = append([]byte(nil), locked.BuildRuntimeDigest...)
			programArchitecture = pgtype.Text{
				String: locked.BuildArchitecture,
				Valid:  true,
			}
		}

		workspaceArtifacts := make(map[string]db.Artifact)
		for _, image := range succeeded.WorkspaceImages {
			artifact, err := createArtifact(
				db.ArtifactKindWorkspaceImage,
				deploymentBuildObjectFromWorkspace(image.Artifact),
			)
			if err != nil {
				return fmt.Errorf(
					"record deployment Workspace %q image: %w",
					image.DeclaredID,
					err,
				)
			}
			workspaceArtifacts[image.DeclaredID] = artifact
		}

		for _, definition := range definitions {
			var workspaceArchitecture pgtype.Text
			var artifactID pgtype.UUID
			if definition.input.Workspace != nil {
				artifact, ok := workspaceArtifacts[definition.input.DeclaredID]
				if !ok {
					return fmt.Errorf(
						"record deployment Workspace %q: image Artifact is missing",
						definition.input.DeclaredID,
					)
				}
				workspaceArchitecture = pgtype.Text{
					String: string(definition.input.Workspace.Architecture),
					Valid:  true,
				}
				artifactID = artifact.ID
			}
			if _, err := work.q.CreateDeploymentDefinition(
				r.Context(),
				db.CreateDeploymentDefinitionParams{
					ID:                    pgvalue.UUID(uuid.Must(uuid.NewV7())),
					EnvironmentID:         environmentID,
					DeploymentID:          deploymentID,
					Kind:                  string(definition.input.Kind),
					DeclaredID:            definition.input.DeclaredID,
					ManifestVersion:       deployment.BuildPlanFormatVersion,
					Manifest:              definition.manifest,
					ManifestDigest:        definition.manifestDigest[:],
					WorkspaceArchitecture: workspaceArchitecture,
					ArtifactID:            artifactID,
				},
			); err != nil {
				return fmt.Errorf(
					"record deployment %s %q: %w",
					definition.input.Kind,
					definition.input.DeclaredID,
					err,
				)
			}
		}
		if result.Logs != nil {
			if err := appendDeploymentBuildLogs(
				r.Context(),
				work.q,
				locked.OrgID,
				locked.ProjectID,
				locked.EnvironmentID,
				locked.DeploymentID,
				*result.Logs,
			); err != nil {
				return errors.New("record deployment build logs")
			}
		}
		row, err := work.q.CompleteDeploymentBuild(r.Context(), db.CompleteDeploymentBuildParams{
			ProgramCodeArtifactID:       programCodeArtifactID,
			ProgramDependencyArtifactID: programDependencyArtifactID,
			ProgramRuntimeDigest:        programRuntimeDigest,
			ProgramArchitecture:         programArchitecture,
			QueueConfig:                 queueConfigJSON,
			OrgID:                       orgID,
			ID:                          deploymentID,
			BuildLeaseID:                pgvalue.UUID(buildLeaseUUID),
			BuildWorkerInstanceID:       buildWorkerInstanceID,
			WorkerEpoch:                 worker.WorkerEpoch,
			LeaseSequence:               request.Lease.LeaseSequence,
			TerminalRequestFingerprint:  fingerprint,
		})
		if isNoRows(err) {
			return conflict(errors.New("deployment build lease is stale"))
		}
		if err != nil {
			return errors.New("mark deployment deployed")
		}
		if err := appendDeploymentLifecycleEvent(r.Context(), work.q, row.OrgID, row.ProjectID, row.EnvironmentID, row.ID, "deployment.deployed", "info", "worker", "deployed", "Deployment build completed"); err != nil {
			return errors.New("record deployment event")
		}
		response = api.WorkerDeploymentBuildResponse{DeploymentID: pgvalue.MustUUIDValue(row.ID).String(), Status: string(row.Status)}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func deploymentBuildResultFingerprint(raw []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("helmr.deployment-build-result.v0\x00"))
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(raw)))
	_, _ = hash.Write(size[:])
	_, _ = hash.Write(raw)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

const maxDeploymentBuildTerminalErrorPayloadBytes = (16 << 10) - 1

func boundedWorkerMessagePayload(message, fallback string) ([]byte, error) {
	fallback = strings.TrimSpace(strings.ToValidUTF8(fallback, "\uFFFD"))
	if fallback == "" {
		fallback = "deployment build failed"
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = fallback
	}
	message = strings.ToValidUTF8(message, "\uFFFD")
	for {
		payload, err := json.Marshal(workerMessagePayload{Message: message})
		if err != nil {
			return nil, err
		}
		if len(payload) <= maxDeploymentBuildTerminalErrorPayloadBytes {
			return payload, nil
		}
		over := len(payload) - maxDeploymentBuildTerminalErrorPayloadBytes
		cut := len(message) - max(over, 1)
		if cut <= 0 {
			payload, err := json.Marshal(workerMessagePayload{Message: fallback})
			if err != nil {
				return nil, err
			}
			if len(payload) > maxDeploymentBuildTerminalErrorPayloadBytes {
				return nil, errors.New("deployment build fallback error exceeds storage limit")
			}
			return payload, nil
		}
		for cut > 0 && !utf8.RuneStart(message[cut]) {
			cut--
		}
		message = strings.TrimSpace(message[:cut])
	}
}

var errManagerAuthorityMismatch = errors.New(
	"build provenance does not match fixed authority",
)

func validateManagerAuthority(
	ctx context.Context,
	store ManagerResolver,
	sourceDigest string,
	selection deployment.SourceSelection,
	target deployment.BuildTarget,
	provenance deployment.BuildProvenance,
) error {
	if store == nil {
		return errors.New("manager store is required")
	}
	if provenance.Architecture != target.Runtime.Architecture ||
		provenance.BuildContractVersion != target.BuildContractVersion ||
		provenance.RuntimeDigest != target.Runtime.Digest ||
		provenance.StandardToolchainDigest != target.StandardToolchainDigest ||
		provenance.Submitted.SourceDigest != sourceDigest ||
		provenance.Submitted.LockfileName != selection.LockfileName ||
		provenance.Submitted.LockfileDigest != selection.LockfileDigest ||
		provenance.Manager.Name != selection.Manager.Name ||
		provenance.Manager.Version != selection.Manager.Version {
		return errManagerAuthorityMismatch
	}
	selector := deployment.NewManagerSelector(
		selection.Manager,
		provenance.Architecture,
	)
	capsule, err := store.Resolve(ctx, selector)
	if err != nil {
		return err
	}
	digest, err := deployment.ManagerCapsuleDigest(capsule)
	if err != nil {
		return fmt.Errorf("digest manager capsule: %w", err)
	}
	if digest != provenance.Manager.CapsuleDigest {
		return errManagerAuthorityMismatch
	}
	return nil
}

func parseDeploymentBuildLeaseIDs(lease api.WorkerDeploymentBuildLease) (pgtype.UUID, pgtype.UUID, pgtype.UUID, pgtype.UUID, error) {
	orgID, err := uuid.Parse(lease.OrgID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, errors.New("deployment build lease org_id must be a UUID")
	}
	projectID, err := uuid.Parse(lease.ProjectID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, errors.New("deployment build lease project_id must be a UUID")
	}
	environmentID, err := uuid.Parse(lease.EnvironmentID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, errors.New("deployment build lease environment_id must be a UUID")
	}
	deploymentID, err := uuid.Parse(lease.DeploymentID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, errors.New("deployment build lease deployment_id must be a UUID")
	}
	return pgvalue.UUID(orgID), pgvalue.UUID(projectID), pgvalue.UUID(environmentID), pgvalue.UUID(deploymentID), nil
}

type deploymentBuildObject struct {
	Digest    string
	SizeBytes int64
	MediaType string
}

func deploymentBuildObjectFromProgram(
	descriptor deployment.ProgramDescriptor,
) deploymentBuildObject {
	return deploymentBuildObject{
		Digest:    descriptor.Digest,
		SizeBytes: descriptor.SizeBytes,
		MediaType: descriptor.MediaType,
	}
}

func deploymentBuildObjectFromWorkspace(
	artifact deployment.WorkspaceImageArtifact,
) deploymentBuildObject {
	return deploymentBuildObject{
		Digest:    artifact.Digest,
		SizeBytes: artifact.SizeBytes,
		MediaType: artifact.MediaType,
	}
}

func deploymentBuildObjects(
	result deployment.BuildSucceeded,
) ([]deploymentBuildObject, error) {
	objects := make([]deploymentBuildObject, 0, 2+len(result.WorkspaceImages))
	if result.ProgramReceipt != nil {
		objects = append(
			objects,
			deploymentBuildObjectFromProgram(result.ProgramReceipt.Code),
			deploymentBuildObjectFromProgram(result.ProgramReceipt.Dependencies),
		)
	}
	for _, image := range result.WorkspaceImages {
		objects = append(objects, deploymentBuildObjectFromWorkspace(image.Artifact))
	}
	unique := make([]deploymentBuildObject, 0, len(objects))
	byDigest := make(map[string]deploymentBuildObject, len(objects))
	for _, object := range objects {
		if existing, ok := byDigest[object.Digest]; ok {
			if existing != object {
				return nil, fmt.Errorf(
					"deployment build reports conflicting metadata for %s",
					object.Digest,
				)
			}
			continue
		}
		byDigest[object.Digest] = object
		unique = append(unique, object)
	}
	return unique, nil
}

func deploymentDefinitionManifest(
	definition deployment.DefinitionInput,
	workspaceImage *deployment.WorkspaceImageArtifact,
) ([]byte, [sha256.Size]byte, error) {
	var manifest any
	switch definition.Kind {
	case deployment.DefinitionKindTask:
		manifest = definition.Task
	case deployment.DefinitionKindActor:
		manifest = definition.Actor
	case deployment.DefinitionKindRunStream:
		manifest = definition.RunStream
	case deployment.DefinitionKindWorkspace:
		if definition.Workspace == nil || workspaceImage == nil {
			return nil, [sha256.Size]byte{}, fmt.Errorf(
				"deployment Workspace %q requires its image result",
				definition.DeclaredID,
			)
		}
		manifest = deployment.WorkspaceManifest{
			Image: deployment.WorkspaceArtifactManifest{
				ArtifactDigest: workspaceImage.Digest,
				MediaType:      workspaceImage.MediaType,
			},
			Resources:    definition.Workspace.Resources,
			Network:      definition.Workspace.Network,
			Architecture: definition.Workspace.Architecture,
		}
	default:
		return nil, [sha256.Size]byte{}, fmt.Errorf(
			"deployment definition kind %q is unsupported",
			definition.Kind,
		)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf(
			"encode deployment %s %q manifest: %w",
			definition.Kind,
			definition.DeclaredID,
			err,
		)
	}
	canonical, digest, err := deployment.CanonicalManifestAndDigest(raw)
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf(
			"canonicalize deployment %s %q manifest: %w",
			definition.Kind,
			definition.DeclaredID,
			err,
		)
	}
	return canonical, digest, nil
}

func createDeploymentBuildArtifact(
	ctx context.Context,
	queries db.Querier,
	orgID pgtype.UUID,
	projectID pgtype.UUID,
	environmentID pgtype.UUID,
	workerInstanceID pgtype.UUID,
	kind db.ArtifactKind,
	object deploymentBuildObject,
) (db.Artifact, error) {
	return queries.CreateArtifact(ctx, db.CreateArtifactParams{
		ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                     orgID,
		ProjectID:                 projectID,
		EnvironmentID:             environmentID,
		Digest:                    object.Digest,
		Kind:                      kind,
		SizeBytes:                 object.SizeBytes,
		MediaType:                 object.MediaType,
		CreatedByWorkerInstanceID: workerInstanceID,
	})
}

func (s *Server) verifyDeploymentBuildArtifacts(
	ctx context.Context,
	objects []deploymentBuildObject,
) error {
	if s.cas == nil {
		return errors.New("deployment build CAS is not configured")
	}
	for _, object := range objects {
		stat, err := s.cas.Stat(ctx, object.Digest)
		if err != nil {
			return fmt.Errorf(
				"deployment build artifact %s is missing from CAS: %w",
				object.Digest,
				err,
			)
		}
		if stat.SizeBytes != object.SizeBytes {
			return &deploymentBuildArtifactMismatch{
				message: fmt.Sprintf("deployment build artifact %s size mismatch", object.Digest),
			}
		}
		if strings.TrimSpace(stat.MediaType) != object.MediaType {
			return &deploymentBuildArtifactMismatch{
				message: fmt.Sprintf("deployment build artifact %s media_type mismatch", object.Digest),
			}
		}
	}
	return nil
}

type deploymentBuildArtifactMismatch struct {
	message string
}

func (err *deploymentBuildArtifactMismatch) Error() string {
	return err.message
}
