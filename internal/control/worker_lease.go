package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
)

const workerLeaseDuration = 5 * time.Minute

func (s *Server) workerLease(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	// Assignment authority is already durable in Postgres. The worker cannot
	// submit capabilities or placement scope while replaying its assignments.
	var request struct{}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, badRequest(fmt.Errorf("invalid worker run lease request JSON: %w", err)))
		return
	}

	worker := workerFromContext(r.Context())
	leasedRun, err := s.db.ClaimAssignedRunLease(r.Context(), db.ClaimAssignedRunLeaseParams{
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
	})
	if isNoRows(err) {
		writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
		return
	}
	if err != nil {
		s.log.Error("claim assigned worker run lease failed", "worker_instance_id", worker.WorkerInstanceID.String(), "worker_epoch", worker.WorkerEpoch, "error", err)
		writeError(w, errors.New("claim assigned run lease"))
		return
	}

	lease := workerRunLeaseResponse(leasedRun)
	run, err := s.workerRunFromLease(r.Context(), leasedRun)
	if err != nil {
		errorPayload, marshalErr := json.Marshal(map[string]string{"message": err.Error()})
		if marshalErr != nil {
			errorPayload = []byte(`{"message":"worker payload unavailable"}`)
		}
		abandonErr := s.db.AbandonLeasedRunLease(r.Context(), db.AbandonLeasedRunLeaseParams{
			ReasonCode: "payload_unavailable", Error: errorPayload,
			OrgID: leasedRun.OrgID, RunID: leasedRun.ID, RunLeaseID: leasedRun.RunLeaseID,
			WorkerGroupID:    leasedRun.RunLeaseWorkerGroupID,
			WorkerInstanceID: leasedRun.RunLeaseWorkerInstanceID, WorkerEpoch: leasedRun.RunLeaseWorkerEpoch,
			AttemptNumber: leasedRun.RunLeaseAttemptNumber, LeaseSequence: leasedRun.RunLeaseSequence,
		})
		if abandonErr != nil {
			s.log.Error("reject worker run payload failed", "run_id", pgvalue.UUIDString(leasedRun.ID), "run_lease_id", pgvalue.UUIDString(leasedRun.RunLeaseID), "error", abandonErr)
			writeError(w, errors.New("reject worker run payload"))
			return
		}
		s.log.Warn("worker run payload unavailable", "run_id", pgvalue.UUIDString(leasedRun.ID), "run_lease_id", pgvalue.UUIDString(leasedRun.RunLeaseID), "error", err)
		writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{Lease: &lease, Run: &run})
}

func (s *Server) workerRejectRun(w http.ResponseWriter, r *http.Request) {
	worker := workerFromContext(r.Context())
	var request api.WorkerRejectRunRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker run rejection JSON: %w", err)))
		return
	}
	lease, err := parseWorkerRunLease(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if lease.workerInstanceID != worker.WorkerInstanceID || lease.workerGroupID != worker.WorkerGroupID || lease.workerEpoch != worker.WorkerEpoch || lease.protocolVersion != worker.ProtocolVersion {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	}
	reason := strings.TrimSpace(request.ReasonCode)
	if reason == "" {
		reason = "worker_preflight_rejected"
	}
	if err := s.db.AbandonLeasedRunLease(r.Context(), db.AbandonLeasedRunLeaseParams{
		ReasonCode: reason, Error: request.Error, OrgID: pgvalue.UUID(lease.orgID), RunID: pgvalue.UUID(lease.runID),
		RunLeaseID: pgvalue.UUID(lease.runLeaseID), WorkerGroupID: lease.workerGroupID,
		WorkerInstanceID: pgvalue.UUID(lease.workerInstanceID), WorkerEpoch: lease.workerEpoch,
		AttemptNumber: lease.attemptNumber, LeaseSequence: lease.leaseSequence,
	}); err != nil {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerRunLeaseForWrite(w http.ResponseWriter, r *http.Request, lease api.WorkerRunLease) (workerActor, workerRunLeaseIDs, bool) {
	leaseIDs, err := parseWorkerRunLease(lease)
	if err != nil {
		writeError(w, badRequest(err))
		return workerActor{}, workerRunLeaseIDs{}, false
	}
	worker := workerFromContext(r.Context())
	if leaseIDs.workerInstanceID != worker.WorkerInstanceID || leaseIDs.workerGroupID != worker.WorkerGroupID || leaseIDs.workerEpoch != worker.WorkerEpoch {
		writeError(w, forbidden(errors.New("worker run lease belongs to another worker epoch")))
		return workerActor{}, workerRunLeaseIDs{}, false
	}
	if err := s.workerCurrentRunningLease(r.Context(), worker, leaseIDs); isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return workerActor{}, workerRunLeaseIDs{}, false
	} else if err != nil {
		s.log.Error("worker run lease lookup failed", "run_id", lease.RunID, "error", err)
		writeError(w, errors.New("get run lease"))
		return workerActor{}, workerRunLeaseIDs{}, false
	}
	return worker, leaseIDs, true
}

type workerRunLeaseIDs struct {
	orgID                 uuid.UUID
	runLeaseID            uuid.UUID
	runID                 uuid.UUID
	workerGroupID         string
	workerInstanceID      uuid.UUID
	workerEpoch           int64
	leaseSequence         int64
	snapshotVersion       int64
	runtimeInstanceID     uuid.UUID
	networkSlotID         uuid.UUID
	networkSlotGeneration int64
	protocolVersion       string
	attemptNumber         int32
}

func parseWorkerRunLease(lease api.WorkerRunLease) (workerRunLeaseIDs, error) {
	orgID, err := uuid.Parse(lease.OrgID)
	if err != nil {
		return workerRunLeaseIDs{}, errors.New("lease.org_id must be a UUID")
	}
	runLeaseID, err := uuid.Parse(lease.ID)
	if err != nil {
		return workerRunLeaseIDs{}, errors.New("lease.id must be a UUID")
	}
	runID, err := uuid.Parse(lease.RunID)
	if err != nil {
		return workerRunLeaseIDs{}, errors.New("lease.run_id must be a UUID")
	}
	workerInstanceID, err := uuid.Parse(lease.WorkerInstanceID)
	if err != nil {
		return workerRunLeaseIDs{}, errors.New("lease.worker_instance_id must be a UUID")
	}
	runtimeInstanceID, err := uuid.Parse(lease.RuntimeInstanceID)
	if err != nil {
		return workerRunLeaseIDs{}, errors.New("lease.runtime_instance_id must be a UUID")
	}
	networkSlotID, err := uuid.Parse(lease.NetworkSlotID)
	if err != nil {
		return workerRunLeaseIDs{}, errors.New("lease.network_slot_id must be a UUID")
	}
	workerGroupID := strings.TrimSpace(lease.WorkerGroupID)
	if workerGroupID == "" || workerGroupID != lease.WorkerGroupID {
		return workerRunLeaseIDs{}, errors.New("lease.worker_group_id must be canonical")
	}
	protocolVersion := strings.TrimSpace(lease.ProtocolVersion)
	if protocolVersion == "" || protocolVersion != lease.ProtocolVersion {
		return workerRunLeaseIDs{}, errors.New("lease.protocol_version must be canonical")
	}
	if lease.WorkerEpoch <= 0 || lease.LeaseSequence <= 0 || lease.SnapshotVersion <= 0 || lease.NetworkSlotGeneration <= 0 || lease.AttemptNumber <= 0 {
		return workerRunLeaseIDs{}, errors.New("lease epoch, sequence, snapshot version, slot generation, and attempt must be positive")
	}
	return workerRunLeaseIDs{
		orgID: orgID, runLeaseID: runLeaseID, runID: runID, workerGroupID: workerGroupID,
		workerInstanceID: workerInstanceID, workerEpoch: lease.WorkerEpoch, leaseSequence: lease.LeaseSequence,
		snapshotVersion:   lease.SnapshotVersion,
		runtimeInstanceID: runtimeInstanceID, networkSlotID: networkSlotID,
		networkSlotGeneration: lease.NetworkSlotGeneration, protocolVersion: protocolVersion,
		attemptNumber: lease.AttemptNumber,
	}, nil
}

func (s *Server) workerExecutionLease(ctx context.Context, worker workerActor, leaseIDs workerRunLeaseIDs) (db.RunLease, error) {
	row, err := s.db.GetStartingRunLease(ctx, db.GetStartingRunLeaseParams{
		OrgID: pgvalue.UUID(leaseIDs.orgID), RunID: pgvalue.UUID(leaseIDs.runID), RunLeaseID: pgvalue.UUID(leaseIDs.runLeaseID),
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
		LeaseSequence: leaseIDs.leaseSequence, RuntimeInstanceID: pgvalue.UUID(leaseIDs.runtimeInstanceID),
		NetworkSlotID: pgvalue.UUID(leaseIDs.networkSlotID), NetworkSlotGeneration: leaseIDs.networkSlotGeneration,
		WorkerProtocolVersion: leaseIDs.protocolVersion,
	})
	if err != nil {
		return db.RunLease{}, err
	}
	if row.TaskAttemptNumber != leaseIDs.attemptNumber {
		return db.RunLease{}, errRecordNotFound
	}
	return row, nil
}

func (s *Server) workerCurrentRunningLease(ctx context.Context, worker workerActor, leaseIDs workerRunLeaseIDs) error {
	row, err := s.db.GetCurrentRunningRunLease(ctx, db.GetCurrentRunningRunLeaseParams{
		OrgID: pgvalue.UUID(leaseIDs.orgID), RunID: pgvalue.UUID(leaseIDs.runID), RunLeaseID: pgvalue.UUID(leaseIDs.runLeaseID),
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
		LeaseSequence: leaseIDs.leaseSequence, RuntimeInstanceID: pgvalue.UUID(leaseIDs.runtimeInstanceID),
		NetworkSlotID: pgvalue.UUID(leaseIDs.networkSlotID), NetworkSlotGeneration: leaseIDs.networkSlotGeneration,
		WorkerProtocolVersion: leaseIDs.protocolVersion,
	})
	if err != nil {
		return err
	}
	if row.TaskAttemptNumber != leaseIDs.attemptNumber {
		return errRecordNotFound
	}
	return nil
}

func workerRunLeaseResponse(row db.ClaimAssignedRunLeaseRow) api.WorkerRunLease {
	return api.WorkerRunLease{
		ID: pgvalue.MustUUIDValue(row.RunLeaseID).String(), OrgID: pgvalue.MustUUIDValue(row.OrgID).String(),
		RunID: pgvalue.MustUUIDValue(row.ID).String(), WorkerGroupID: row.RunLeaseWorkerGroupID,
		WorkerInstanceID: pgvalue.MustUUIDValue(row.RunLeaseWorkerInstanceID).String(), WorkerEpoch: row.RunLeaseWorkerEpoch,
		LeaseSequence: row.RunLeaseSequence, SnapshotVersion: row.StateVersion,
		RuntimeInstanceID: pgvalue.MustUUIDValue(row.RunLeaseRuntimeInstanceID).String(),
		NetworkSlotID:     pgvalue.MustUUIDValue(row.RunLeaseNetworkSlotID).String(), NetworkSlotGeneration: row.RunLeaseNetworkSlotGeneration,
		ProtocolVersion: row.RunLeaseWorkerProtocolVersion, AttemptNumber: row.RunLeaseAttemptNumber,
		Trace:     api.TraceContext{TraceID: row.RunLeaseTraceID.String, SpanID: row.RunLeaseSpanID.String, Traceparent: row.RunLeaseTraceparent.String},
		ExpiresAt: pgvalue.Time(row.RunLeaseExpiresAt),
	}
}

func (s *Server) workerRunFromLease(ctx context.Context, row db.ClaimAssignedRunLeaseRow) (api.WorkerRun, error) {
	restore, err := s.workerRestorePayload(ctx, row)
	if err != nil {
		return api.WorkerRun{}, err
	}
	secretNames, err := deploymentTaskSecretNames(row.DeploymentTaskSecretDeclarations)
	if err != nil {
		return api.WorkerRun{}, terminalPayload("secret_unavailable", err)
	}
	var resolvedSecrets api.ResolvedSecrets
	if len(secretNames) > 0 && restore == nil {
		if s.secrets == nil {
			return api.WorkerRun{}, errors.New("secret store is not configured")
		}
		resolvedSecrets, err = s.secrets.ResolveScopedNames(ctx, pgvalue.MustUUIDValue(row.OrgID), pgvalue.MustUUIDValue(row.ProjectID), pgvalue.MustUUIDValue(row.EnvironmentID), secretNames)
		if err != nil {
			if secret.IsUnavailable(err) || isNoRows(err) {
				return api.WorkerRun{}, terminalPayload("secret_unavailable", err)
			}
			return api.WorkerRun{}, err
		}
	}
	requirements, err := workerRunRequirementsFromLease(row)
	if err != nil {
		return api.WorkerRun{}, err
	}
	sessionID, err := requiredUUIDString(row.SessionID, "session_id")
	if err != nil {
		return api.WorkerRun{}, err
	}
	return api.WorkerRun{
		ID: pgvalue.MustUUIDValue(row.ID).String(), Version: row.RunDeploymentVersion, DeploymentVersion: row.RunDeploymentVersion,
		APIVersion: row.RunApiVersion, SDKVersion: row.RunSdkVersion, CLIVersion: row.RunCliVersion,
		WorkerProtocolVersion: row.RunLeaseWorkerProtocolVersion, AttemptNumber: row.RunLeaseAttemptNumber,
		RunLeaseID: pgvalue.MustUUIDValue(row.RunLeaseID).String(), SnapshotVersion: row.StateVersion,
		SessionID: sessionID, TaskID: row.TaskID, Payload: json.RawMessage(row.Payload), Secrets: resolvedSecrets,
		DeploymentSource: api.DeploymentSourceArtifact{Digest: row.DeploymentSourceDigest, MediaType: api.DeploymentSourceArtifactMediaType},
		DeploymentTask: api.WorkerDeploymentTask{ID: pgvalue.MustUUIDValue(row.DeploymentTaskID).String(), FilePath: row.DeploymentTaskFilePath,
			ExportName: row.DeploymentTaskExportName, HandlerEntrypoint: row.DeploymentTaskHandlerEntrypoint,
			BundleDigest: row.DeploymentTaskBundleDigest, BundleFormatVersion: row.DeploymentTaskBundleFormatVersion},
		Workspace: workerWorkspaceFromLease(row), Requirements: requirements,
		MaxDurationSeconds: activeDurationMsToSeconds(row.MaxActiveDurationMs), ActiveDurationMs: row.ActiveDurationMs,
		Trace:   api.TraceContext{TraceID: row.RunLeaseTraceID.String, SpanID: row.RunLeaseSpanID.String, Traceparent: row.RunLeaseTraceparent.String},
		Restore: restore,
	}, nil
}

func workerWorkspaceFromLease(row db.ClaimAssignedRunLeaseRow) api.WorkerWorkspace {
	workspace := api.WorkerWorkspace{
		ID: pgvalue.MustUUIDValue(row.WorkspaceID).String(), WorkspaceMountID: pgvalue.MustUUIDValue(row.WorkspaceMountID).String(),
		FencingGeneration: row.WorkspaceMountFencingGeneration, WriteLeaseID: pgvalue.MustUUIDValue(row.WorkspaceLeaseID).String(),
		WriteFencingToken: row.WorkspaceFencingToken, MountPath: row.WorkspaceMountPath,
		Artifact: &api.WorkerWorkspaceArtifact{Digest: row.WorkspaceArtifactDigest, MediaType: row.WorkspaceArtifactMediaType,
			Encoding: row.WorkspaceArtifactEncoding, SizeBytes: row.WorkspaceArtifactSizeBytes, EntryCount: row.WorkspaceArtifactEntryCount},
		SubstrateSource: &api.WorkerRuntimeSubstrateSource{
			DeploymentSandboxID:        pgvalue.MustUUIDValue(row.WorkspaceDeploymentSandboxID).String(),
			SandboxImageArtifact:       api.CASObject{Digest: row.WorkspaceSandboxImageArtifactDigest, SizeBytes: row.WorkspaceSandboxImageArtifactSizeBytes, MediaType: row.WorkspaceSandboxImageArtifactMediaType},
			SandboxImageArtifactFormat: row.WorkspaceSandboxImageArtifactFormat, RootfsDigest: row.WorkspaceSandboxRootfsDigest,
			ImageDigest: row.WorkspaceSandboxImageDigest, ImageFormat: row.WorkspaceSandboxImageFormat, WorkspaceMountPath: row.WorkspaceMountPath,
			RuntimeABI: row.WorkspaceRuntimeAbi, GuestdABI: row.WorkspaceGuestdAbi, AdapterABI: row.WorkspaceAdapterAbi,
		},
	}
	if row.WorkspaceBaseVersionID.Valid {
		workspace.BaseVersionID = pgvalue.MustUUIDValue(row.WorkspaceBaseVersionID).String()
	}
	if row.WorkspaceRuntimeSubstrateID.Valid {
		workspace.SubstrateSource.RuntimeSubstrate = &api.WorkerRuntimeSubstrate{
			ID: pgvalue.MustUUIDValue(row.WorkspaceRuntimeSubstrateID).String(), DeploymentSandboxID: pgvalue.MustUUIDValue(row.WorkspaceDeploymentSandboxID).String(),
			Artifact:        api.CASObject{Digest: row.WorkspaceRuntimeSubstrateBlobDigest.String, SizeBytes: row.WorkspaceRuntimeSubstrateBlobSizeBytes.Int64, MediaType: row.WorkspaceRuntimeSubstrateBlobMediaType.String},
			SubstrateDigest: row.WorkspaceRuntimeSubstrateDigest.String, Format: row.WorkspaceRuntimeSubstrateFormat.String,
			BuilderABI: row.WorkspaceRuntimeSubstrateBuilderAbi.String, LayoutABI: row.WorkspaceRuntimeSubstrateLayoutAbi.String,
			SizeBytes: row.WorkspaceRuntimeSubstrateSizeBytes.Int64,
		}
	}
	return workspace
}

func workerRunRequirementsFromLease(row db.ClaimAssignedRunLeaseRow) (compute.RunRuntimeRequirements, error) {
	return compute.RunRuntimeRequirementsFromFields(compute.RunRuntimeRequirementFields{
		RequestedMilliCPU: row.RequestedMilliCpu, RequestedMemoryMiB: int64(row.RequestedMemoryMib),
		RequestedDiskMiB: int64(row.RequestedDiskMib), RequestedExecutionSlots: row.RequestedExecutionSlots,
		RuntimeID: row.RequirementsRuntimeID, RuntimeArch: row.RequirementsRuntimeArch, RuntimeABI: row.RequirementsRuntimeAbi,
		KernelDigest: row.RequirementsKernelDigest, InitramfsDigest: row.RequirementsInitramfsDigest, RootfsDigest: row.RequirementsRootfsDigest,
		CNIProfile: row.RequirementsCniProfile, NetworkPolicyJSON: row.RequirementsNetworkPolicy,
		NetworkPolicyLabel: "worker run network policy", PlacementJSON: row.RequirementsPlacement, PlacementLabel: "worker run placement",
	})
}
