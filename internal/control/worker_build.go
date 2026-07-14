package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	deploymentBuildLeaseDuration = 30 * time.Minute
	currentGuestdABI             = "helmr.guestd.v0"
	currentAdapterABI            = "helmr.adapter.v0"
)

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
	row, err := s.db.ClaimNextDeploymentBuildLease(r.Context(), db.ClaimNextDeploymentBuildLeaseParams{
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
		ExpiresAt: pgvalue.Timestamptz(leaseExpiresAt),
	})
	if isNoRows(err) {
		writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildLeaseResponse{})
		return
	}
	if err != nil {
		s.log.Error("claim assigned deployment build failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("claim assigned deployment build"))
		return
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
		BuildAttemptNumber:         row.BuildAttemptNumber,
		LeaseSequence:              row.LeaseSequence,
		WorkerProtocolVersion:      row.WorkerProtocolVersion,
		ExpiresAt:                  leaseExpiresAt,
		RequestedWorkloadDiskBytes: row.RequestedWorkloadDiskBytes,
		RequestedScratchBytes:      row.RequestedScratchBytes,
		RequestedCPUMillis:         row.RequestedCpuMillis,
		RequestedMemoryBytes:       row.RequestedMemoryBytes,
		RequestedBuildExecutors:    row.RequestedBuildExecutors,
	}
	deployment := api.WorkerDeploymentBuild{
		ID:                    deploymentID,
		Version:               row.Version,
		APIVersion:            row.ApiVersion,
		SDKVersion:            row.SdkVersion,
		CLIVersion:            row.CliVersion,
		BundleFormatVersion:   row.BundleFormatVersion,
		WorkerProtocolVersion: row.WorkerProtocolVersion,
		ProjectID:             pgvalue.MustUUIDValue(row.ProjectID).String(),
		EnvironmentID:         pgvalue.MustUUIDValue(row.EnvironmentID).String(),
		DeploymentSource: api.DeploymentSourceArtifact{
			Digest:    row.DeploymentSourceDigest,
			SizeBytes: row.SourceSizeBytes,
			MediaType: row.SourceMediaType,
		},
	}
	writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildLeaseResponse{Lease: &lease, Deployment: &deployment})
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
	if err != nil || lease.WorkerGroupID != worker.WorkerGroupID || lease.WorkerInstanceID != worker.WorkerInstanceID.String() || lease.WorkerEpoch != worker.WorkerEpoch || lease.BuildAttemptNumber <= 0 || lease.LeaseSequence <= 0 || lease.WorkerProtocolVersion != worker.ProtocolVersion {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	}
	started, err := s.db.GetStartedDeploymentBuildLease(r.Context(), db.GetStartedDeploymentBuildLeaseParams{
		OrgID: orgID, DeploymentID: deploymentID, BuildLeaseID: pgvalue.UUID(leaseID),
		BuildAttemptNumber: lease.BuildAttemptNumber, LeaseSequence: lease.LeaseSequence,
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
		BuildLeaseID: pgvalue.UUID(leaseID), BuildAttemptNumber: lease.BuildAttemptNumber,
		LeaseSequence: lease.LeaseSequence, WorkerGroupID: worker.WorkerGroupID,
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
	if err != nil || lease.WorkerGroupID != worker.WorkerGroupID || lease.WorkerInstanceID != worker.WorkerInstanceID.String() || lease.WorkerEpoch != worker.WorkerEpoch || lease.BuildAttemptNumber <= 0 || lease.LeaseSequence <= 0 || lease.WorkerProtocolVersion != worker.ProtocolVersion {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	}
	expiresAt := time.Now().Add(deploymentBuildLeaseDuration)
	renewed, err := s.db.RenewDeploymentBuildLease(r.Context(), db.RenewDeploymentBuildLeaseParams{
		ExpiresAt: pgvalue.Timestamptz(expiresAt), OrgID: orgID, DeploymentID: deploymentID,
		BuildLeaseID: pgvalue.UUID(leaseID), BuildAttemptNumber: lease.BuildAttemptNumber,
		LeaseSequence: lease.LeaseSequence, WorkerGroupID: worker.WorkerGroupID,
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
	var request api.WorkerDeploymentBuildRejectRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker deployment build reject JSON: %w", err)))
		return
	}
	lease := request.Lease
	orgID, _, _, deploymentID, err := parseDeploymentBuildLeaseIDs(lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	leaseID, err := uuid.Parse(strings.TrimSpace(lease.ID))
	if err != nil || lease.WorkerGroupID != worker.WorkerGroupID || lease.WorkerInstanceID != worker.WorkerInstanceID.String() || lease.WorkerEpoch != worker.WorkerEpoch || lease.BuildAttemptNumber <= 0 || lease.LeaseSequence <= 0 || lease.WorkerProtocolVersion != worker.ProtocolVersion {
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
		BuildAttemptNumber: lease.BuildAttemptNumber, LeaseSequence: lease.LeaseSequence,
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
	_, err = s.db.RejectDeploymentBuildLease(r.Context(), db.RejectDeploymentBuildLeaseParams{
		ReasonCode: pgtype.Text{String: reason, Valid: true}, Error: request.Error, OrgID: orgID, DeploymentID: deploymentID,
		BuildLeaseID: pgvalue.UUID(leaseID), BuildAttemptNumber: lease.BuildAttemptNumber, LeaseSequence: lease.LeaseSequence,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
		TerminalRequestFingerprint: fingerprint,
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("reject deployment build"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerCompleteDeploymentBuild(w http.ResponseWriter, r *http.Request) {
	worker := workerFromContext(r.Context())
	if s.db == nil {
		writeError(w, unavailable(errors.New("deployment build storage is not configured")))
		return
	}
	var request api.WorkerCompleteDeploymentBuildRequest
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
	if err != nil || request.Lease.WorkerGroupID != worker.WorkerGroupID || request.Lease.WorkerEpoch != worker.WorkerEpoch || request.Lease.BuildAttemptNumber <= 0 || request.Lease.LeaseSequence <= 0 {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	}
	fingerprint, err := terminalRequestFingerprint("deployment_build.complete", request.Result)
	if err != nil {
		writeError(w, errors.New("fingerprint deployment build completion"))
		return
	}
	terminal, err := s.db.GetDeploymentBuildTerminalResult(r.Context(), db.GetDeploymentBuildTerminalResultParams{
		OrgID: orgID, DeploymentID: deploymentID, BuildLeaseID: pgvalue.UUID(buildLeaseUUID),
		BuildAttemptNumber: request.Lease.BuildAttemptNumber, LeaseSequence: request.Lease.LeaseSequence,
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
	if time.Now().After(request.Lease.ExpiresAt) {
		writeError(w, conflict(errors.New("deployment build lease expired")))
		return
	}

	buildWorkerInstanceID := pgvalue.UUID(worker.WorkerInstanceID)
	var response api.WorkerDeploymentBuildResponse
	err = s.inTx(r.Context(), func(work *txWork) error {
		failBuild := func(message string) error {
			message = strings.TrimSpace(message)
			if message == "" {
				message = "deployment build failed"
			}
			payload, err := json.Marshal(workerMessagePayload{Message: message})
			if err != nil {
				return errors.New("marshal deployment build error")
			}
			row, err := work.q.FailDeploymentBuild(r.Context(), db.FailDeploymentBuildParams{
				Failure: payload, ReasonCode: pgtype.Text{String: "worker_reported_failure", Valid: true},
				TerminalRequestFingerprint: fingerprint,
				OrgID:                      orgID, ID: deploymentID, BuildLeaseID: pgvalue.UUID(buildLeaseUUID),
				BuildWorkerInstanceID: buildWorkerInstanceID,
				WorkerEpoch:           worker.WorkerEpoch, BuildAttemptNumber: request.Lease.BuildAttemptNumber,
				LeaseSequence: request.Lease.LeaseSequence,
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
		if request.Result.Error != nil {
			return failBuild(*request.Result.Error)
		}
		casObjects, err := deployment.ValidateBuildResult(request.Result)
		if err != nil {
			return failBuild(err.Error())
		}
		if err := s.verifyDeploymentBuildArtifacts(r.Context(), casObjects); err != nil {
			return failBuild(err.Error())
		}
		buildDeployment, err := work.q.GetDeploymentBuildLease(r.Context(), db.GetDeploymentBuildLeaseParams{
			OrgID: orgID, ID: deploymentID, BuildLeaseID: pgvalue.UUID(buildLeaseUUID),
			BuildWorkerInstanceID: buildWorkerInstanceID,
			WorkerEpoch:           worker.WorkerEpoch,
		})
		if isNoRows(err) {
			return conflict(errors.New("deployment build lease is stale"))
		}
		if err != nil {
			return errors.New("get deployment build lease")
		}
		if buildDeployment.WorkerGroupID != worker.WorkerGroupID {
			return forbidden(errors.New("deployment build lease belongs to another worker group"))
		}
		workerGroupID := buildDeployment.WorkerGroupID
		workerState, err := work.q.GetWorkerInstanceState(r.Context(), db.GetWorkerInstanceStateParams{
			ID:            buildWorkerInstanceID,
			WorkerGroupID: workerGroupID,
		})
		if isNoRows(err) {
			return failBuild("deployment build worker instance was not found")
		}
		if err != nil {
			return errors.New("get deployment build worker state")
		}
		casObjectByDigest := make(map[string]api.CASObject, len(casObjects))
		for _, object := range casObjects {
			casObjectByDigest[strings.TrimSpace(object.Digest)] = object
			if _, err := work.q.UpsertCasObject(r.Context(), db.UpsertCasObjectParams{
				OrgID:     orgID,
				Digest:    object.Digest,
				SizeBytes: object.SizeBytes,
				MediaType: object.MediaType,
			}); err != nil {
				return failBuild("record deployment build artifact: " + err.Error())
			}
		}
		buildManifestArtifact, err := createDeploymentBuildArtifact(r.Context(), work.q, orgID, projectID, environmentID, buildWorkerInstanceID, strings.TrimSpace(request.Result.BuildManifestDigest), db.ArtifactKindBuildManifest, casObjectByDigest)
		if err != nil {
			return failBuild("record build manifest artifact: " + err.Error())
		}
		deploymentManifestArtifact, err := createDeploymentBuildArtifact(r.Context(), work.q, orgID, projectID, environmentID, buildWorkerInstanceID, strings.TrimSpace(request.Result.DeploymentManifestDigest), db.ArtifactKindDeploymentManifest, casObjectByDigest)
		if err != nil {
			return failBuild("record deployment manifest artifact: " + err.Error())
		}
		queueConcurrencyLimits := map[string]*int32{}
		for _, queue := range request.Result.Queues {
			queueName := strings.TrimSpace(queue.Name)
			queueConcurrencyLimits[queueName] = queue.ConcurrencyLimit
			if _, err := work.q.CreateDeploymentQueue(r.Context(), db.CreateDeploymentQueueParams{
				ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:            orgID,
				ProjectID:        projectID,
				EnvironmentID:    environmentID,
				DeploymentID:     deploymentID,
				Name:             queueName,
				ConcurrencyLimit: pgvalue.Int4Ptr(queue.ConcurrencyLimit),
			}); err != nil {
				return failBuild("record deployment queue: " + err.Error())
			}
		}
		deploymentSandboxIDs := map[string]pgtype.UUID{}
		for _, task := range request.Result.Tasks {
			bundleArtifact, err := createDeploymentBuildArtifact(r.Context(), work.q, orgID, projectID, environmentID, buildWorkerInstanceID, strings.TrimSpace(task.BundleDigest), db.ArtifactKindTaskBundle, casObjectByDigest)
			if err != nil {
				return failBuild("record task bundle artifact: " + err.Error())
			}
			secretDeclarations, err := json.Marshal(task.Secrets)
			if err != nil {
				return failBuild("encode deployment task secrets: " + err.Error())
			}
			scheduleDeclarations, err := json.Marshal(task.Schedules)
			if err != nil {
				return failBuild("encode deployment task schedules: " + err.Error())
			}
			networkPolicy, err := json.Marshal(task.Network)
			if err != nil {
				return failBuild("encode deployment task network: " + err.Error())
			}
			sandboxID := strings.TrimSpace(task.SandboxID)
			deploymentSandboxID, ok := deploymentSandboxIDs[sandboxID]
			if !ok {
				imageArtifact, err := createDeploymentBuildArtifact(r.Context(), work.q, orgID, projectID, environmentID, buildWorkerInstanceID, strings.TrimSpace(task.SandboxImageArtifact.Digest), db.ArtifactKindSandboxImage, casObjectByDigest)
				if err != nil {
					return failBuild("record deployment sandbox image artifact: " + err.Error())
				}
				resourceFloor, err := json.Marshal(map[string]any{
					"milli_cpu":  task.RequestedMilliCPU,
					"memory_mib": task.RequestedMemoryMiB,
					"disk_mib":   task.RequestedDiskMiB,
				})
				if err != nil {
					return failBuild("encode deployment sandbox resource floor: " + err.Error())
				}
				fingerprint, err := deploymentSandboxContractFingerprint(deploymentSandboxContractFingerprintInput{
					RootfsDigest:       pgvalue.TextValue(workerState.RootfsDigest),
					RuntimeABI:         pgvalue.TextValue(workerState.RuntimeABI),
					GuestdABI:          currentGuestdABI,
					AdapterABI:         currentAdapterABI,
					WorkspaceMountPath: strings.TrimSpace(task.WorkspaceMountPath),
					NetworkPolicy:      task.Network,
					FilesystemFormat:   strings.TrimSpace(task.FilesystemFormat),
					ContractVersion:    1,
				})
				if err != nil {
					return failBuild("fingerprint deployment sandbox contract: " + err.Error())
				}
				var sandboxPublicID string
				row, err := createWithPublicID(r.Context(), []publicIDSlot{{prefix: publicid.Sandbox, value: &sandboxPublicID}}, func() (db.DeploymentSandbox, error) {
					return work.q.CreateDeploymentSandbox(r.Context(), db.CreateDeploymentSandboxParams{
						ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
						PublicID:            sandboxPublicID,
						OrgID:               orgID,
						ProjectID:           projectID,
						EnvironmentID:       environmentID,
						DeploymentID:        deploymentID,
						SandboxID:           sandboxID,
						ImageArtifactID:     imageArtifact.ID,
						ImageArtifactFormat: strings.TrimSpace(task.SandboxImageArtifactFormat),
						RootfsDigest:        pgvalue.TextValue(workerState.RootfsDigest),
						ImageDigest:         imageArtifact.Digest,
						ImageFormat:         strings.TrimSpace(task.SandboxImageFormat),
						WorkspaceMountPath:  strings.TrimSpace(task.WorkspaceMountPath),
						ResourceFloor:       resourceFloor,
						DiskFloorMib:        task.RequestedDiskMiB,
						NetworkPolicy:       networkPolicy,
						RuntimeABI:          pgvalue.TextValue(workerState.RuntimeABI),
						GuestdAbi:           currentGuestdABI,
						AdapterAbi:          currentAdapterABI,
						FilesystemFormat:    strings.TrimSpace(task.FilesystemFormat),
						DefaultUid:          pgtype.Int8{},
						DefaultGid:          pgtype.Int8{},
						DefaultWorkdir:      "",
						ContractVersion:     1,
						Fingerprint:         fingerprint,
					})
				})
				if err != nil {
					return failBuild("record deployment sandbox: " + err.Error())
				}
				deploymentSandboxID = row.ID
				deploymentSandboxIDs[sandboxID] = deploymentSandboxID
			}
			queueName := strings.TrimSpace(task.QueueName)
			queueConcurrencyLimit, ok := queueConcurrencyLimits[queueName]
			if !ok {
				return failBuild("deployment task references undefined queue")
			}
			retryPolicy, err := normalizedRetryPolicy(task.RetryPolicy)
			if err != nil {
				return failBuild("validate deployment task retry policy: " + err.Error())
			}
			var deploymentTaskPublicID, taskPublicID string
			if _, err := createWithPublicID(r.Context(), []publicIDSlot{
				{prefix: publicid.DeploymentTask, value: &deploymentTaskPublicID},
				{prefix: publicid.Task, value: &taskPublicID},
			}, func() (db.DeploymentTask, error) {
				return work.q.CreateDeploymentTask(r.Context(), db.CreateDeploymentTaskParams{
					ID:                    pgvalue.UUID(uuid.Must(uuid.NewV7())),
					PublicID:              deploymentTaskPublicID,
					OrgID:                 orgID,
					ProjectID:             projectID,
					EnvironmentID:         environmentID,
					DeploymentID:          deploymentID,
					DeploymentSandboxID:   deploymentSandboxID,
					TaskID:                strings.TrimSpace(task.TaskID),
					TaskPublicID:          taskPublicID,
					FilePath:              strings.TrimSpace(task.FilePath),
					ExportName:            strings.TrimSpace(task.ExportName),
					HandlerEntrypoint:     strings.TrimSpace(task.HandlerEntrypoint),
					BundleArtifactID:      bundleArtifact.ID,
					BundleFormatVersion:   firstPositiveInt32(task.BundleFormatVersion, api.CurrentBundleFormatVersion),
					RequestedMilliCpu:     task.RequestedMilliCPU,
					RequestedMemoryMib:    task.RequestedMemoryMiB,
					RequestedDiskMib:      task.RequestedDiskMiB,
					SecretDeclarations:    secretDeclarations,
					ResourceRequirements:  []byte("{}"),
					NetworkPolicy:         networkPolicy,
					ScheduleDeclarations:  scheduleDeclarations,
					QueueName:             queueName,
					QueueConcurrencyLimit: pgvalue.Int4Ptr(queueConcurrencyLimit),
					Ttl:                   strings.TrimSpace(task.TTL),
					MaxActiveDurationMs:   int64(task.MaxDurationSeconds) * 1000,
					RetryPolicy:           retryPolicy,
				})
			}); err != nil {
				return failBuild("record deployment task: " + err.Error())
			}
		}
		for _, stream := range request.Result.Streams {
			schemaJSON := stream.SchemaJSON
			if len(schemaJSON) == 0 {
				schemaJSON = []byte("null")
			}
			if _, err := work.q.UpsertDeploymentStream(r.Context(), db.UpsertDeploymentStreamParams{
				ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:             orgID,
				ProjectID:         projectID,
				EnvironmentID:     environmentID,
				DeploymentID:      deploymentID,
				Name:              strings.TrimSpace(stream.Name),
				Direction:         db.StreamDirection(strings.TrimSpace(stream.Direction)),
				SchemaFingerprint: strings.TrimSpace(stream.SchemaFingerprint),
				SchemaJson:        schemaJSON,
				Metadata:          []byte("{}"),
			}); err != nil {
				return failBuild("record deployment stream: " + err.Error())
			}
		}
		row, err := work.q.CompleteDeploymentBuild(r.Context(), db.CompleteDeploymentBuildParams{
			BuildManifestArtifactID:      buildManifestArtifact.ID,
			DeploymentManifestArtifactID: deploymentManifestArtifact.ID,
			OrgID:                        orgID,
			ID:                           deploymentID,
			BuildLeaseID:                 pgvalue.UUID(buildLeaseUUID),
			BuildWorkerInstanceID:        buildWorkerInstanceID,
			WorkerEpoch:                  worker.WorkerEpoch,
			BuildAttemptNumber:           request.Lease.BuildAttemptNumber,
			LeaseSequence:                request.Lease.LeaseSequence,
			TerminalRequestFingerprint:   fingerprint,
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

type deploymentSandboxContractFingerprintInput struct {
	RootfsDigest       string
	RuntimeABI         string
	GuestdABI          string
	AdapterABI         string
	WorkspaceMountPath string
	NetworkPolicy      compute.NetworkPolicy
	FilesystemFormat   string
	DefaultUID         pgtype.Int8
	DefaultGID         pgtype.Int8
	DefaultWorkdir     string
	ContractVersion    int32
}

type deploymentSandboxContractFingerprintDocument struct {
	AdapterABI         string                         `json:"adapter_abi"`
	ContractVersion    int32                          `json:"contract_version"`
	DefaultGID         *int64                         `json:"default_gid"`
	DefaultUID         *int64                         `json:"default_uid"`
	DefaultWorkdir     string                         `json:"default_workdir"`
	FilesystemFormat   string                         `json:"filesystem_format"`
	GuestdABI          string                         `json:"guestd_abi"`
	NetworkPolicy      deploymentSandboxNetworkPolicy `json:"network_policy"`
	RootfsDigest       string                         `json:"rootfs_digest"`
	RuntimeABI         string                         `json:"runtime_abi"`
	WorkspaceMountPath string                         `json:"workspace_mount_path"`
}

type deploymentSandboxNetworkPolicy struct {
	Allow    []string `json:"allow"`
	Deny     []string `json:"deny"`
	Internet bool     `json:"internet"`
}

func deploymentSandboxContractFingerprint(input deploymentSandboxContractFingerprintInput) (string, error) {
	network := input.NetworkPolicy
	network.Allow = append([]string(nil), network.Allow...)
	network.Deny = append([]string(nil), network.Deny...)
	sort.Strings(network.Allow)
	sort.Strings(network.Deny)
	document := deploymentSandboxContractFingerprintDocument{
		AdapterABI:       strings.TrimSpace(input.AdapterABI),
		ContractVersion:  input.ContractVersion,
		DefaultWorkdir:   strings.TrimSpace(input.DefaultWorkdir),
		FilesystemFormat: strings.TrimSpace(input.FilesystemFormat),
		GuestdABI:        strings.TrimSpace(input.GuestdABI),
		NetworkPolicy: deploymentSandboxNetworkPolicy{
			Allow:    network.Allow,
			Deny:     network.Deny,
			Internet: network.Internet,
		},
		RootfsDigest:       strings.TrimSpace(input.RootfsDigest),
		RuntimeABI:         strings.TrimSpace(input.RuntimeABI),
		WorkspaceMountPath: strings.TrimSpace(input.WorkspaceMountPath),
	}
	if input.DefaultUID.Valid {
		value := input.DefaultUID.Int64
		document.DefaultUID = &value
	}
	if input.DefaultGID.Valid {
		value := input.DefaultGID.Int64
		document.DefaultGID = &value
	}
	if document.RootfsDigest == "" {
		return "", errors.New("rootfs_digest is required")
	}
	if document.RuntimeABI == "" {
		return "", errors.New("runtime_abi is required")
	}
	if document.GuestdABI == "" {
		return "", errors.New("guestd_abi is required")
	}
	if document.AdapterABI == "" {
		return "", errors.New("adapter_abi is required")
	}
	if document.WorkspaceMountPath == "" {
		return "", errors.New("workspace_mount_path is required")
	}
	if document.FilesystemFormat == "" {
		return "", errors.New("filesystem_format is required")
	}
	if document.ContractVersion <= 0 {
		return "", errors.New("contract_version is required")
	}
	body, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	canonical, err := canonicalJSON(body)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func createDeploymentBuildArtifact(ctx context.Context, queries db.Querier, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, workerInstanceID pgtype.UUID, digest string, kind db.ArtifactKind, objects map[string]api.CASObject) (db.Artifact, error) {
	object, ok := objects[strings.TrimSpace(digest)]
	if !ok {
		return db.Artifact{}, fmt.Errorf("missing CAS object %s", digest)
	}
	return queries.CreateArtifact(ctx, db.CreateArtifactParams{
		ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                     orgID,
		ProjectID:                 projectID,
		EnvironmentID:             environmentID,
		Digest:                    strings.TrimSpace(object.Digest),
		Kind:                      kind,
		SizeBytes:                 object.SizeBytes,
		MediaType:                 object.MediaType,
		CreatedByWorkerInstanceID: workerInstanceID,
	})
}

func (s *Server) verifyDeploymentBuildArtifacts(ctx context.Context, objects []api.CASObject) error {
	if s.cas == nil {
		return nil
	}
	for _, object := range objects {
		stat, err := s.cas.Stat(ctx, object.Digest)
		if err != nil {
			return fmt.Errorf("deployment build artifact %s is missing from CAS: %w", object.Digest, err)
		}
		if stat.SizeBytes != object.SizeBytes {
			return fmt.Errorf("deployment build artifact %s size mismatch", object.Digest)
		}
		if strings.TrimSpace(stat.MediaType) != object.MediaType {
			return fmt.Errorf("deployment build artifact %s media_type mismatch", object.Digest)
		}
	}
	return nil
}
