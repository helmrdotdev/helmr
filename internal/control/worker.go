package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	workerLeaseDuration          = 5 * time.Minute
	deploymentBuildLeaseDuration = 30 * time.Minute
	defaultWorkerTokenTTL        = 15 * time.Minute
)

func (s *Server) workerRegister(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("worker bootstrap storage is not configured"))
		return
	}
	if len(s.authSecret) == 0 {
		writeError(w, http.StatusServiceUnavailable, errors.New("worker bootstrap is not configured"))
		return
	}
	var request api.WorkerRegisterRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker bootstrap request JSON: %w", err))
		return
	}
	registrationHash, err := auth.HashToken(s.authSecret, request.BootstrapToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, errors.New("worker bootstrap token is required"))
		return
	}
	if strings.TrimSpace(request.BootstrapToken) == s.workerRegisterToken {
		if err := s.ensureWorkerBootstrapToken(r.Context(), s.db); err != nil {
			s.log.Error("worker bootstrap token bootstrap failed", "error", err)
			writeError(w, http.StatusInternalServerError, errors.New("configure worker bootstrap token"))
			return
		}
	}
	generated, err := auth.GenerateWorkerInstanceSecret(s.authSecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("generate worker instance credential"))
		return
	}
	workerInstanceID := ids.New()
	resourceID := strings.TrimSpace(request.ResourceID)
	if resourceID == "" {
		resourceID = workerInstanceID.String()
	}
	credential, err := s.db.CreateWorkerInstanceCredentialFromBootstrap(r.Context(), db.CreateWorkerInstanceCredentialFromBootstrapParams{
		BootstrapTokenHash: registrationHash,
		CredentialID:       ids.ToPG(ids.New()),
		WorkerInstanceID:   ids.ToPG(workerInstanceID),
		ResourceID:         resourceID,
		KeyPrefix:          generated.KeyPrefix,
		SecretHash:         generated.TokenHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, errors.New("worker bootstrap token is invalid"))
		return
	}
	if err != nil {
		s.log.Error("worker bootstrap failed", "worker_instance_id", workerInstanceID.String(), "resource_id", resourceID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("register worker"))
		return
	}
	writeJSON(w, http.StatusCreated, api.WorkerRegisterResponse{
		WorkerInstanceID:     ids.MustFromPG(credential.WorkerInstanceID).String(),
		WorkerGroupID:        ids.MustFromPG(credential.WorkerGroupID).String(),
		WorkerInstanceSecret: generated.Raw,
	})
}

func (s *Server) workerAuthToken(w http.ResponseWriter, r *http.Request) {
	if s.db == nil || len(s.authSecret) == 0 || len(s.workerTokenSecret) == 0 {
		writeError(w, http.StatusServiceUnavailable, errors.New("worker authentication is not configured"))
		return
	}
	var request api.WorkerTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker token request JSON: %w", err))
		return
	}
	request.WorkerInstanceID = strings.TrimSpace(request.WorkerInstanceID)
	if request.WorkerInstanceID == "" {
		writeError(w, http.StatusBadRequest, errors.New("worker_instance_id is required"))
		return
	}
	workerInstanceID, err := ids.Parse(request.WorkerInstanceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("worker_instance_id must be a UUID"))
		return
	}
	secretHash, err := auth.HashToken(s.authSecret, request.WorkerInstanceSecret)
	if err != nil {
		writeError(w, http.StatusUnauthorized, errors.New("worker authentication is required"))
		return
	}
	credential, err := s.db.AuthenticateWorkerInstanceCredential(r.Context(), db.AuthenticateWorkerInstanceCredentialParams{
		WorkerInstanceID: ids.ToPG(workerInstanceID),
		SecretHash:       secretHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, errors.New("worker authentication is required"))
		return
	}
	if err != nil {
		s.log.Error("worker instance credential authentication failed", "worker_instance_id", request.WorkerInstanceID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("worker authentication"))
		return
	}
	credentialID, err := ids.FromPG(credential.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("worker instance credential id"))
		return
	}
	now := time.Now()
	expiresAt := now.Add(s.workerTokenTTL)
	signed, err := auth.IssueWorkerToken(s.workerTokenSecret, auth.WorkerClaims{
		WorkerInstanceID: ids.MustFromPG(credential.WorkerInstanceID).String(),
		CredentialID:     credentialID.String(),
		IssuedAt:         now,
		ExpiresAt:        expiresAt,
	})
	if err != nil {
		s.log.Error("mint worker token failed", "worker_instance_id", request.WorkerInstanceID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("mint worker token"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerTokenResponse{
		Token:            signed,
		ExpiresInSeconds: int64(s.workerTokenTTL / time.Second),
	})
}

func (s *Server) workerLeaseDeploymentBuild(w http.ResponseWriter, r *http.Request) {
	worker := workerFromContext(r.Context())
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("deployment build storage is not configured"))
		return
	}
	var request api.WorkerDeploymentBuildLeaseRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker deployment build lease JSON: %w", err))
		return
	}
	capabilities, err := normalizeWorkerCapabilities(request.Capabilities)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.db.UpsertWorkerInstanceHeartbeat(r.Context(), workerInstanceHeartbeatParams(worker, capabilities)); err != nil {
		s.log.Error("worker heartbeat failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("record worker heartbeat"))
		return
	}
	if err := s.db.EnsureRuntimeReleaseSelection(r.Context(), capabilities.RuntimeID); err != nil {
		s.log.Error("ensure runtime release selection failed", "worker_instance_id", worker.WorkerInstanceID.String(), "runtime_id", capabilities.RuntimeID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("select runtime release"))
		return
	}
	capacity, err := s.db.GetWorkerInstanceQueueCapacity(r.Context(), ids.ToPG(worker.WorkerInstanceID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildLeaseResponse{})
		return
	}
	if err != nil {
		s.log.Error("worker capacity lookup failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get worker capacity"))
		return
	}
	if capacity.AvailableExecutionSlots <= 0 || capacity.AvailableMilliCpu <= 0 || capacity.AvailableMemoryMib <= 0 {
		writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildLeaseResponse{})
		return
	}
	leaseID := ids.New().String()
	leaseExpiresAt := time.Now().Add(deploymentBuildLeaseDuration)
	row, err := s.db.LeaseQueuedDeploymentBuild(r.Context(), db.LeaseQueuedDeploymentBuildParams{
		WorkerGroupID:         ids.ToPG(worker.WorkerGroupID),
		BuildLeaseID:          pgtype.Text{String: leaseID, Valid: true},
		BuildWorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
		BuildLeaseExpiresAt:   pgtype.Timestamptz{Time: leaseExpiresAt, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildLeaseResponse{})
		return
	}
	if err != nil {
		s.log.Error("worker deployment build lease failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("lease deployment build"))
		return
	}
	deploymentID := ids.MustFromPG(row.ID).String()
	lease := api.WorkerDeploymentBuildLease{
		ID:               leaseID,
		OrgID:            ids.MustFromPG(row.OrgID).String(),
		ProjectID:        ids.MustFromPG(row.ProjectID).String(),
		EnvironmentID:    ids.MustFromPG(row.EnvironmentID).String(),
		DeploymentID:     deploymentID,
		WorkerInstanceID: worker.WorkerInstanceID.String(),
		ExpiresAt:        leaseExpiresAt,
	}
	deployment := api.WorkerDeploymentBuild{
		ID:                    deploymentID,
		Version:               row.Version,
		APIVersion:            row.ApiVersion,
		SDKVersion:            row.SdkVersion,
		CLIVersion:            row.CliVersion,
		BundleFormatVersion:   row.BundleFormatVersion,
		WorkerProtocolVersion: row.WorkerProtocolVersion,
		ProjectID:             ids.MustFromPG(row.ProjectID).String(),
		EnvironmentID:         ids.MustFromPG(row.EnvironmentID).String(),
		DeploymentSource: api.DeploymentSourceArtifact{
			Digest:    row.DeploymentSourceDigest,
			SizeBytes: row.SourceSizeBytes,
			MediaType: row.SourceMediaType,
		},
	}
	writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildLeaseResponse{Lease: &lease, Deployment: &deployment})
}

func (s *Server) workerCompleteDeploymentBuild(w http.ResponseWriter, r *http.Request) {
	worker := workerFromContext(r.Context())
	if s.db == nil || s.tx == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("deployment build storage is not configured"))
		return
	}
	var request api.WorkerCompleteDeploymentBuildRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker deployment build completion JSON: %w", err))
		return
	}
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, http.StatusConflict, errors.New("deployment build lease is stale"))
		return
	}
	if time.Now().After(request.Lease.ExpiresAt) {
		writeError(w, http.StatusConflict, errors.New("deployment build lease expired"))
		return
	}
	orgID, projectID, environmentID, deploymentID, err := parseDeploymentBuildLeaseIDs(request.Lease)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	tx, err := s.tx.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("begin deployment build completion"))
		return
	}
	defer tx.Rollback(r.Context())
	queries := db.New(tx)
	buildWorkerInstanceID := ids.ToPG(worker.WorkerInstanceID)
	failBuild := func(message string) bool {
		payload, err := json.Marshal(workerMessagePayload{Message: strings.TrimSpace(message)})
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("marshal deployment build error"))
			return false
		}
		row, err := queries.FailDeploymentBuild(r.Context(), db.FailDeploymentBuildParams{
			Failure:               payload,
			OrgID:                 orgID,
			ProjectID:             projectID,
			EnvironmentID:         environmentID,
			ID:                    deploymentID,
			BuildLeaseID:          pgtype.Text{String: strings.TrimSpace(request.Lease.ID), Valid: true},
			BuildWorkerInstanceID: buildWorkerInstanceID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusConflict, errors.New("deployment build lease is stale"))
			return false
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("mark deployment build failed"))
			return false
		}
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("commit deployment build failure"))
			return false
		}
		writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildResponse{DeploymentID: ids.MustFromPG(row.ID).String(), Status: string(row.Status)})
		return true
	}
	if request.Result.Error != nil {
		failBuild(*request.Result.Error)
		return
	}
	casObjects, err := deployment.ValidateBuildResult(request.Result)
	if err != nil {
		failBuild(err.Error())
		return
	}
	if err := s.verifyDeploymentBuildArtifacts(r.Context(), casObjects); err != nil {
		failBuild(err.Error())
		return
	}
	if _, err := queries.GetDeploymentBuildLease(r.Context(), db.GetDeploymentBuildLeaseParams{
		OrgID:                 orgID,
		ProjectID:             projectID,
		EnvironmentID:         environmentID,
		ID:                    deploymentID,
		BuildLeaseID:          pgtype.Text{String: strings.TrimSpace(request.Lease.ID), Valid: true},
		BuildWorkerInstanceID: buildWorkerInstanceID,
	}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("deployment build lease is stale"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("get deployment build lease"))
		return
	}
	casObjectByDigest := make(map[string]api.CASObject, len(casObjects))
	for _, object := range casObjects {
		casObjectByDigest[strings.TrimSpace(object.Digest)] = object
		if _, err := queries.UpsertCasObject(r.Context(), db.UpsertCasObjectParams{
			Digest:    object.Digest,
			SizeBytes: object.SizeBytes,
			MediaType: object.MediaType,
		}); err != nil {
			failBuild("record deployment build artifact: " + err.Error())
			return
		}
	}
	buildManifestArtifact, err := createDeploymentBuildArtifact(r.Context(), queries, orgID, projectID, environmentID, buildWorkerInstanceID, strings.TrimSpace(request.Result.BuildManifestDigest), db.ArtifactKindBuildManifest, casObjectByDigest)
	if err != nil {
		failBuild("record build manifest artifact: " + err.Error())
		return
	}
	deploymentManifestArtifact, err := createDeploymentBuildArtifact(r.Context(), queries, orgID, projectID, environmentID, buildWorkerInstanceID, strings.TrimSpace(request.Result.DeploymentManifestDigest), db.ArtifactKindDeploymentManifest, casObjectByDigest)
	if err != nil {
		failBuild("record deployment manifest artifact: " + err.Error())
		return
	}
	for _, task := range request.Result.Tasks {
		bundleArtifact, err := createDeploymentBuildArtifact(r.Context(), queries, orgID, projectID, environmentID, buildWorkerInstanceID, strings.TrimSpace(task.BundleDigest), db.ArtifactKindTaskBundle, casObjectByDigest)
		if err != nil {
			failBuild("record task bundle artifact: " + err.Error())
			return
		}
		secretDeclarations, err := json.Marshal(task.Secrets)
		if err != nil {
			failBuild("encode deployment task secrets: " + err.Error())
			return
		}
		scheduleDeclarations, err := json.Marshal(task.Schedules)
		if err != nil {
			failBuild("encode deployment task schedules: " + err.Error())
			return
		}
		networkPolicy, err := json.Marshal(task.Network)
		if err != nil {
			failBuild("encode deployment task network: " + err.Error())
			return
		}
		if _, err := queries.CreateDeploymentTask(r.Context(), db.CreateDeploymentTaskParams{
			ID:                    ids.ToPG(ids.New()),
			OrgID:                 orgID,
			ProjectID:             projectID,
			EnvironmentID:         environmentID,
			DeploymentID:          deploymentID,
			TaskID:                strings.TrimSpace(task.TaskID),
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
			QueueName:             strings.TrimSpace(task.QueueName),
			QueueConcurrencyLimit: pgInt4Ptr(task.ConcurrencyLimit),
			Ttl:                   strings.TrimSpace(task.TTL),
			MaxDurationSeconds:    task.MaxDurationSeconds,
		}); err != nil {
			failBuild("record deployment task: " + err.Error())
			return
		}
	}
	row, err := queries.CompleteDeploymentBuild(r.Context(), db.CompleteDeploymentBuildParams{
		BuildManifestArtifactID:      buildManifestArtifact.ID,
		DeploymentManifestArtifactID: deploymentManifestArtifact.ID,
		OrgID:                        orgID,
		ProjectID:                    projectID,
		EnvironmentID:                environmentID,
		ID:                           deploymentID,
		BuildLeaseID:                 pgtype.Text{String: strings.TrimSpace(request.Lease.ID), Valid: true},
		BuildWorkerInstanceID:        buildWorkerInstanceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("deployment build lease is stale"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("mark deployment deployed"))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("commit deployment build completion"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildResponse{DeploymentID: ids.MustFromPG(row.ID).String(), Status: string(row.Status)})
}

func parseDeploymentBuildLeaseIDs(lease api.WorkerDeploymentBuildLease) (pgtype.UUID, pgtype.UUID, pgtype.UUID, pgtype.UUID, error) {
	orgID, err := ids.Parse(lease.OrgID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, errors.New("deployment build lease org_id must be a UUID")
	}
	projectID, err := ids.Parse(lease.ProjectID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, errors.New("deployment build lease project_id must be a UUID")
	}
	environmentID, err := ids.Parse(lease.EnvironmentID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, errors.New("deployment build lease environment_id must be a UUID")
	}
	deploymentID, err := ids.Parse(lease.DeploymentID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, errors.New("deployment build lease deployment_id must be a UUID")
	}
	return ids.ToPG(orgID), ids.ToPG(projectID), ids.ToPG(environmentID), ids.ToPG(deploymentID), nil
}

func createDeploymentBuildArtifact(ctx context.Context, queries *db.Queries, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, workerInstanceID pgtype.UUID, digest string, kind db.ArtifactKind, objects map[string]api.CASObject) (db.Artifact, error) {
	object, ok := objects[strings.TrimSpace(digest)]
	if !ok {
		return db.Artifact{}, fmt.Errorf("missing CAS object %s", digest)
	}
	return queries.CreateArtifact(ctx, db.CreateArtifactParams{
		ID:                        ids.ToPG(ids.New()),
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

func (s *Server) workerLease(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	if s.dispatchQueue == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run queue item queue is not configured"))
		return
	}
	var request api.WorkerRunLeaseRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker run lease request JSON: %w", err))
		return
	}
	capabilities, err := normalizeWorkerCapabilities(request.Capabilities)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	worker := workerFromContext(r.Context())
	if _, err := s.db.UpsertWorkerInstanceHeartbeat(r.Context(), workerInstanceHeartbeatParams(worker, capabilities)); err != nil {
		s.log.Error("worker heartbeat failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("record worker heartbeat"))
		return
	}
	if err := s.db.EnsureRuntimeReleaseSelection(r.Context(), capabilities.RuntimeID); err != nil {
		s.log.Error("ensure runtime release selection failed", "worker_instance_id", worker.WorkerInstanceID.String(), "runtime_id", capabilities.RuntimeID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("select runtime release"))
		return
	}
	capacity, err := s.db.GetWorkerInstanceQueueCapacity(r.Context(), ids.ToPG(worker.WorkerInstanceID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
		return
	}
	if err != nil {
		s.log.Error("worker capacity lookup failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get worker capacity"))
		return
	}
	if capacity.AvailableExecutionSlots <= 0 || capacity.AvailableMilliCpu <= 0 || capacity.AvailableMemoryMib <= 0 {
		writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
		return
	}
	runClaimer, err := dispatch.NewClaimer(s.db, s.dispatchQueue)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run queue item queue is not configured"))
		return
	}
	dequeueRequest := dispatch.DequeueRequest{
		WorkerInstanceID: worker.WorkerInstanceID.String(),
		Available: compute.ResourceVector{
			MilliCPU:  capacity.AvailableMilliCpu,
			MemoryMiB: capacity.AvailableMemoryMib,
			DiskMiB:   capacity.AvailableDiskMib,
			Slots:     capacity.AvailableExecutionSlots,
		},
		Runtime: compute.RuntimeSelector{
			ID:              capabilities.RuntimeID,
			Arch:            capabilities.RuntimeArch,
			ABI:             capabilities.RuntimeABI,
			KernelDigest:    capabilities.KernelDigest,
			InitramfsDigest: capabilities.InitramfsDigest,
			RootfsDigest:    capabilities.RootfsDigest,
			CNIProfile:      capabilities.CNIProfile,
		},
		Region:      capabilities.Region,
		Labels:      capabilities.Labels,
		MaxMessages: 1,
	}
	var queueLease dispatch.ClaimedRun
	var leasedRun db.LeaseRunExecutionRow
	const scopePageSize int32 = 100
	scanSeed := int32(s.workerLeaseScanSeed.Add(1) & 0x7fffffff)
	scopeSelector := dispatch.RoundRobinQueueScopeSelector{}
	foundLease := false
	for rowOffset := int32(0); !foundLease; rowOffset += scopePageSize {
		scopeRows, err := s.db.ListQueueScopes(r.Context(), db.ListQueueScopesParams{
			WorkerGroupID: ids.ToPG(worker.WorkerGroupID),
			ScanSeed:      fmt.Sprint(scanSeed),
			RowOffset:     rowOffset,
			RowLimit:      scopePageSize,
		})
		if err != nil {
			s.log.Error("worker queue scope lookup failed", "error", err)
			writeError(w, http.StatusInternalServerError, errors.New("list worker queue scopes"))
			return
		}
		if len(scopeRows) == 0 {
			break
		}
		scopes := make([]dispatch.QueueScope, 0, len(scopeRows))
		for _, row := range scopeRows {
			scopes = append(scopes, dispatch.QueueScope{OrgID: row.OrgID, QueueName: row.QueueName})
		}
		// Worker leasing exits after one claim, so keep scope ordering page-bounded.
		scopes = scopeSelector.Order(scopes)
		for _, scope := range scopes {
			orgID := ids.MustFromPG(scope.OrgID)
			if err := dispatch.SweepExpiredForOrg(r.Context(), s.db, scope.OrgID); err != nil {
				s.log.Warn("sweep expired executions failed", "org_id", orgID.String(), "error", err)
			}
			dequeueRequest.OrgID = orgID.String()
			for _, queueName := range dispatch.QueueNamesForRuntime(scope.QueueName, dequeueRequest.Runtime) {
				dequeueRequest.QueueName = queueName
				candidateLease, err := runClaimer.Claim(r.Context(), dispatch.ClaimRequest{DequeueRequest: dequeueRequest})
				if errors.Is(err, dispatch.ErrNoClaim) {
					continue
				}
				if err != nil {
					s.log.Error("worker queue lease failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
					writeError(w, http.StatusInternalServerError, errors.New("lease run queue item"))
					return
				}
				if candidateLease.Lease.MessageID == "" {
					continue
				}
				candidateRun, err := s.db.LeaseRunExecution(r.Context(), db.LeaseRunExecutionParams{
					OrgID:             candidateLease.Entry.OrgID,
					RunID:             candidateLease.Entry.RunID,
					WorkerInstanceID:  ids.ToPG(worker.WorkerInstanceID),
					ExecutionID:       ids.ToPG(ids.New()),
					DispatchMessageID: pgtype.Text{String: candidateLease.Lease.MessageID, Valid: true},
					DispatchLeaseID:   candidateLease.Lease.ID,
					DispatchAttempt:   candidateLease.Lease.AttemptNumber,
					LeaseExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(workerLeaseDuration), Valid: true},
				})
				if err == nil {
					queueLease = candidateLease
					leasedRun = candidateRun
					foundLease = true
					break
				}
				if errors.Is(err, pgx.ErrNoRows) {
					s.requeueWorkerQueueItem(r.Context(), worker, candidateLease.Entry.RunID, candidateLease.Lease, dispatch.NackReasonLeaseConflict, "execution lease conflict")
					continue
				}
				s.requeueWorkerQueueItem(r.Context(), worker, candidateLease.Entry.RunID, candidateLease.Lease, dispatch.NackReasonRetry, err.Error())
				s.log.Error("worker run lease failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
				writeError(w, http.StatusInternalServerError, errors.New("lease run"))
				return
			}
			if foundLease {
				break
			}
		}
		if int32(len(scopeRows)) < scopePageSize {
			break
		}
	}
	if !foundLease {
		writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
		return
	}

	lease := workerRunLeaseResponse(leasedRun)
	run, err := s.workerRunFromLease(r.Context(), leasedRun)
	if err != nil {
		if failure, ok := terminalPayloadFailure(err); ok {
			if failErr := s.failLeasedRunPayload(r.Context(), leasedRun, queueLease.Lease, failure); failErr != nil {
				s.log.Error("fail worker run payload failed", "run_id", ids.MustFromPG(leasedRun.ID).String(), "execution_id", ids.MustFromPG(leasedRun.ExecutionID).String(), "error", failErr)
				writeError(w, http.StatusInternalServerError, errors.New("fail worker run payload"))
				return
			}
			s.log.Warn("terminal worker run payload failed", "run_id", ids.MustFromPG(leasedRun.ID).String(), "execution_id", ids.MustFromPG(leasedRun.ExecutionID).String(), "failure_kind", failure.kind, "error", err)
			writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
			return
		}
		if abandonErr := s.db.AbandonLeasedRunExecution(r.Context(), db.AbandonLeasedRunExecutionParams{
			OrgID:            leasedRun.OrgID,
			RunID:            leasedRun.ID,
			ExecutionID:      leasedRun.ExecutionID,
			WorkerInstanceID: leasedRun.ExecutionWorkerInstanceID,
		}); abandonErr != nil {
			s.log.Error("abandon worker run lease failed", "run_id", ids.MustFromPG(leasedRun.ID).String(), "execution_id", ids.MustFromPG(leasedRun.ExecutionID).String(), "error", abandonErr)
		}
		s.requeueWorkerQueueItem(r.Context(), worker, leasedRun.ID, queueLease.Lease, dispatch.NackReasonRetry, err.Error())
		s.log.Error("build worker run payload failed", "run_id", ids.MustFromPG(leasedRun.ID).String(), "execution_id", ids.MustFromPG(leasedRun.ExecutionID).String(), "error", err)
		writeError(w, http.StatusBadGateway, errors.New("build worker run payload"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{Lease: &lease, Run: &run})
}

func (s *Server) workerActivate(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	var request api.WorkerActivateRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker activate request JSON: %w", err))
		return
	}
	capabilities, err := normalizeWorkerCapabilities(request.Capabilities)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	worker := workerFromContext(r.Context())
	if _, err := s.db.UpsertWorkerInstanceHeartbeat(r.Context(), workerInstanceHeartbeatParams(worker, capabilities)); err != nil {
		s.log.Error("worker activate failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("activate worker"))
		return
	}
	if err := s.db.EnsureRuntimeReleaseSelection(r.Context(), capabilities.RuntimeID); err != nil {
		s.log.Error("ensure runtime release selection failed", "worker_instance_id", worker.WorkerInstanceID.String(), "runtime_id", capabilities.RuntimeID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("select runtime release"))
		return
	}
	if _, err := s.db.SetWorkerInstanceStatus(r.Context(), db.SetWorkerInstanceStatusParams{
		ID:     ids.ToPG(worker.WorkerInstanceID),
		Status: db.WorkerInstanceStatusActive,
	}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("worker is not registered"))
		return
	} else if err != nil {
		s.log.Error("worker activate status failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("activate worker"))
		return
	}
	s.writeWorkerStatus(w, r, worker)
}

func (s *Server) workerDrain(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	worker := workerFromContext(r.Context())
	if _, err := s.db.SetWorkerInstanceStatus(r.Context(), db.SetWorkerInstanceStatusParams{
		ID:     ids.ToPG(worker.WorkerInstanceID),
		Status: db.WorkerInstanceStatusDraining,
	}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("worker is not registered"))
		return
	} else if err != nil {
		s.log.Error("worker drain failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("drain worker"))
		return
	}
	s.writeWorkerStatus(w, r, worker)
}

func (s *Server) workerStatus(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	s.writeWorkerStatus(w, r, workerFromContext(r.Context()))
}

func (s *Server) writeWorkerStatus(w http.ResponseWriter, r *http.Request, worker workerActor) {
	state, err := s.db.GetWorkerInstanceState(r.Context(), ids.ToPG(worker.WorkerInstanceID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("worker is not registered"))
		return
	}
	if err != nil {
		s.log.Error("get worker status failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get worker status"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerStatusResponse{
		WorkerInstanceID: ids.MustFromPG(state.ID).String(),
		WorkerGroupID:    ids.MustFromPG(state.WorkerGroupID).String(),
		Status:           api.WorkerStatus(state.Status),
		ActiveExecutions: state.ActiveExecutions,
	})
}

func (s *Server) workerStart(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	var request api.WorkerStartRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker start request JSON: %w", err))
		return
	}
	leaseIDs, err := parseWorkerRunLease(request.Lease)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, http.StatusForbidden, errors.New("worker run lease belongs to another worker"))
		return
	}
	if _, _, err := s.workerExecutionLease(r.Context(), worker, leaseIDs); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	} else if err != nil {
		s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get queue lease"))
		return
	}
	status, err := s.db.StartRunExecution(r.Context(), db.StartRunExecutionParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		ExecutionID:      ids.ToPG(leaseIDs.executionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	}
	if err != nil {
		s.log.Error("worker start failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("start run"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerStartResponse{RunID: request.Lease.RunID, Status: string(status)})
}

func (s *Server) workerRenew(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	if s.dispatchQueue == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run queue item queue is not configured"))
		return
	}
	var request api.WorkerRenewRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker renew request JSON: %w", err))
		return
	}
	leaseIDs, err := parseWorkerRunLease(request.Lease)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, http.StatusForbidden, errors.New("worker run lease belongs to another worker"))
		return
	}
	leaseRow, queueLease, err := s.workerExecutionLease(r.Context(), worker, leaseIDs)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
			return
		}
		s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get queue lease"))
		return
	}
	expiresAt := time.Now().Add(workerLeaseDuration)
	if _, err := s.db.RenewRunQueueReservation(r.Context(), db.RenewRunQueueReservationParams{
		OrgID:                ids.ToPG(leaseIDs.orgID),
		RunID:                ids.ToPG(leaseIDs.runID),
		WorkerInstanceID:     ids.ToPG(worker.WorkerInstanceID),
		DispatchMessageID:    pgtype.Text{String: leaseRow.DispatchMessageID, Valid: true},
		ReservationExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	} else if err != nil {
		s.log.Error("worker queue lease renewal failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("renew queue lease"))
		return
	}
	renewed, err := s.db.RenewRunExecutionLease(r.Context(), db.RenewRunExecutionLeaseParams{
		OrgID:             ids.ToPG(leaseIDs.orgID),
		RunID:             ids.ToPG(leaseIDs.runID),
		ExecutionID:       ids.ToPG(leaseIDs.executionID),
		WorkerInstanceID:  ids.ToPG(worker.WorkerInstanceID),
		DispatchMessageID: leaseRow.DispatchMessageID,
		DispatchLeaseID:   leaseRow.DispatchLeaseID,
		LeaseExpiresAt:    pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	}
	if err != nil {
		s.log.Error("worker renew failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("renew run execution"))
		return
	}
	if _, err := s.dispatchQueue.Renew(r.Context(), queueLease, expiresAt); err != nil {
		s.log.Warn("worker dispatch renew repair failed", "run_id", request.Lease.RunID, "error", err)
	}
	lease := api.WorkerRunLease{
		ID:                request.Lease.ID,
		OrgID:             request.Lease.OrgID,
		RunID:             request.Lease.RunID,
		WorkerInstanceID:  ids.MustFromPG(renewed.WorkerInstanceID).String(),
		AttemptNumber:     renewed.AttemptNumber,
		DispatchMessageID: renewed.DispatchMessageID,
		DispatchLeaseID:   renewed.DispatchLeaseID,
		ExpiresAt:         pgTime(renewed.LeaseExpiresAt),
	}
	writeJSON(w, http.StatusOK, api.WorkerRenewResponse{Lease: lease})
}

func (s *Server) workerRelease(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	if s.dispatchQueue == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run queue item queue is not configured"))
		return
	}
	var request api.WorkerReleaseRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker release request JSON: %w", err))
		return
	}
	leaseIDs, err := parseWorkerRunLease(request.Lease)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, http.StatusForbidden, errors.New("worker run lease belongs to another worker"))
		return
	}
	_, lease, err := s.workerExecutionLease(r.Context(), worker, leaseIDs)
	activeQueueLeaseFound := err == nil
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
			writeError(w, http.StatusInternalServerError, errors.New("get queue lease"))
			return
		}
	}
	status, exitCode, errorMessage, err := releaseFields(request.Result)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	output := releaseOutput(request.Result, status, exitCode)
	terminalEventKind, terminalEventPayload, err := terminalRunEventForFields(status, exitCode, errorMessage, request.Result)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode terminal run event"))
		return
	}
	run, err := s.db.ReleaseRunExecution(r.Context(), db.ReleaseRunExecutionParams{
		OrgID:                ids.ToPG(leaseIDs.orgID),
		RunID:                ids.ToPG(leaseIDs.runID),
		ExecutionID:          ids.ToPG(leaseIDs.executionID),
		WorkerInstanceID:     ids.ToPG(worker.WorkerInstanceID),
		DispatchMessageID:    leaseIDs.queueMessageID,
		DispatchLeaseID:      leaseIDs.queueLeaseID,
		Status:               status,
		ExitCode:             exitCode,
		Output:               output,
		ErrorMessage:         errorMessage,
		TerminalEventKind:    terminalEventKind,
		TerminalEventPayload: terminalEventPayload,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	}
	if err != nil {
		s.log.Error("worker release failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("release run"))
		return
	}
	if activeQueueLeaseFound {
		s.ackWorkerQueueLease(r.Context(), ids.ToPG(leaseIDs.runID), lease)
	}
	writeJSON(w, http.StatusOK, api.WorkerReleaseResponse{RunID: request.Lease.RunID, Status: string(run.Status)})
}

func (s *Server) ackWorkerQueueLease(ctx context.Context, runID pgtype.UUID, lease dispatch.Lease) {
	if err := s.dispatchQueue.Ack(ctx, lease); err != nil {
		s.log.Warn("complete queue lease failed", "run_id", ids.MustFromPG(runID).String(), "error", err)
	}
}

func (s *Server) requeueWorkerQueueItem(ctx context.Context, worker workerActor, runID pgtype.UUID, lease dispatch.Lease, reason dispatch.NackReason, lastError string) {
	orgID, err := ids.Parse(lease.Message.OrgID)
	if err != nil {
		s.log.Warn("requeue run queue item failed", "run_id", ids.MustFromPG(runID).String(), "reason", reason, "error", err)
		if nackErr := s.dispatchQueue.Nack(ctx, lease, dispatch.NackReasonInvalid); nackErr != nil {
			s.log.Warn("requeue queue lease failed", "run_id", ids.MustFromPG(runID).String(), "reason", dispatch.NackReasonInvalid, "error", nackErr)
		}
		return
	}
	if _, err := s.db.RequeueRunQueueItem(ctx, db.RequeueRunQueueItemParams{
		OrgID:             ids.ToPG(orgID),
		RunID:             runID,
		WorkerInstanceID:  ids.ToPG(worker.WorkerInstanceID),
		DispatchMessageID: pgtype.Text{String: lease.MessageID, Valid: true},
		LastError:         strings.TrimSpace(lastError),
	}); err != nil {
		s.log.Warn("requeue run queue item failed", "run_id", ids.MustFromPG(runID).String(), "reason", reason, "error", err)
		nackReason := reason
		if errors.Is(err, pgx.ErrNoRows) {
			nackReason = dispatch.NackReasonInvalid
		}
		if nackErr := s.dispatchQueue.Nack(ctx, lease, nackReason); nackErr != nil {
			s.log.Warn("requeue queue lease failed", "run_id", ids.MustFromPG(runID).String(), "reason", nackReason, "error", nackErr)
		}
		return
	}
	if err := s.dispatchQueue.Nack(ctx, lease, dispatch.NackReasonInvalid); err != nil {
		s.log.Warn("discard stale queue lease failed", "run_id", ids.MustFromPG(runID).String(), "reason", reason, "error", err)
	}
}

func (s *Server) workerAppendLogs(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerAppendLogRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker log request JSON: %w", err))
		return
	}
	content, err := base64.StdEncoding.DecodeString(request.ContentBase64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("log content is not valid base64"))
		return
	}
	kind := "log.stdout"
	switch request.Stream {
	case api.WorkerLogStreamStdout:
	case api.WorkerLogStreamStderr:
		kind = "log.stderr"
	default:
		writeError(w, http.StatusBadRequest, errors.New("stream must be stdout or stderr"))
		return
	}
	if request.ObservedSeq > uint64(^uint64(0)>>1) {
		writeError(w, http.StatusBadRequest, errors.New("observed_seq is too large"))
		return
	}
	worker, leaseIDs, ok := s.workerRunLeaseForWrite(w, r, request.Lease)
	if !ok {
		return
	}
	payload, err := json.Marshal(workerLogChunkPayload{
		RunID:       request.Lease.RunID,
		Stream:      request.Stream,
		ObservedSeq: request.ObservedSeq,
		Bytes:       len(content),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode worker log event"))
		return
	}
	_, err = s.db.AppendRunLogChunk(r.Context(), db.AppendRunLogChunkParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		ExecutionID:      ids.ToPG(leaseIDs.executionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
		Stream:           db.RunLogStream(request.Stream),
		ObservedSeq:      int64(request.ObservedSeq),
		Content:          content,
		Kind:             kind,
		Payload:          payload,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	}
	if err != nil {
		s.log.Error("append worker logs failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("append worker logs"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerEventResponse{RunID: request.Lease.RunID})
}

func (s *Server) workerRecordLogEntry(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRecordLogEntryRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker log entry request JSON: %w", err))
		return
	}
	payload, err := json.Marshal(workerMessagePayload{Message: request.Entry})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode worker log entry"))
		return
	}
	s.appendWorkerEvent(w, r, request.Lease, "log", payload)
}

func (s *Server) workerEmitEvent(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerEmitEventRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker event request JSON: %w", err))
		return
	}
	request.EventType = strings.TrimSpace(request.EventType)
	if request.EventType == "" {
		writeError(w, http.StatusBadRequest, errors.New("event_type is required"))
		return
	}
	content := request.Content
	if len(content) == 0 {
		content = json.RawMessage(`null`)
	}
	if !json.Valid(content) {
		writeError(w, http.StatusBadRequest, errors.New("content must be valid JSON"))
		return
	}
	payload, err := json.Marshal(workerEmitPayload{
		Type:    request.EventType,
		Content: content,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode worker event"))
		return
	}
	s.appendWorkerEvent(w, r, request.Lease, "emit."+request.EventType, payload)
}

func (s *Server) appendWorkerEvent(w http.ResponseWriter, r *http.Request, lease api.WorkerRunLease, kind string, payload []byte) {
	worker, leaseIDs, ok := s.workerRunLeaseForWrite(w, r, lease)
	if !ok {
		return
	}
	_, err := s.db.AppendRunEventForExecution(r.Context(), db.AppendRunEventForExecutionParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		ExecutionID:      ids.ToPG(leaseIDs.executionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
		Kind:             kind,
		Payload:          payload,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	}
	if err != nil {
		s.log.Error("append worker event failed", "run_id", lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("append worker event"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerEventResponse{RunID: lease.RunID})
}

func (s *Server) workerRunLeaseForWrite(w http.ResponseWriter, r *http.Request, lease api.WorkerRunLease) (workerActor, workerRunLeaseIDs, bool) {
	leaseIDs, err := parseWorkerRunLease(lease)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return workerActor{}, workerRunLeaseIDs{}, false
	}
	worker := workerFromContext(r.Context())
	if lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, http.StatusForbidden, errors.New("worker run lease belongs to another worker"))
		return workerActor{}, workerRunLeaseIDs{}, false
	}
	if _, _, err := s.workerExecutionLease(r.Context(), worker, leaseIDs); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return workerActor{}, workerRunLeaseIDs{}, false
	} else if err != nil {
		s.log.Error("worker queue lease lookup failed", "run_id", lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get queue lease"))
		return workerActor{}, workerRunLeaseIDs{}, false
	}
	return worker, leaseIDs, true
}

type payloadFailure struct {
	kind    string
	message string
}

type workerMessagePayload struct {
	Message string `json:"message"`
}

type workerLogChunkPayload struct {
	Bytes       int                 `json:"bytes"`
	ObservedSeq uint64              `json:"observed_seq"`
	RunID       string              `json:"run_id"`
	Stream      api.WorkerLogStream `json:"stream"`
}

type workerEmitPayload struct {
	Content json.RawMessage `json:"content"`
	Type    string          `json:"type"`
}

type workerHeartbeatPayload struct {
	CNIProfile      string `json:"cni_profile"`
	InitramfsDigest string `json:"initramfs_digest"`
	KernelDigest    string `json:"kernel_digest"`
	RootfsDigest    string `json:"rootfs_digest"`
	RuntimeABI      string `json:"runtime_abi"`
	RuntimeArch     string `json:"runtime_arch"`
	RuntimeID       string `json:"runtime_id"`
}

type runCompletedPayload struct {
	ExitCode int32 `json:"exit_code"`
}

type runFailurePayload struct {
	Detail      any    `json:"detail"`
	FailureKind string `json:"failure_kind"`
}

type taskFailedDetailPayload struct {
	ExitCode int32 `json:"exit_code"`
}

type workerFailureDetailPayload struct {
	LimitSeconds *int32 `json:"limit_seconds,omitempty"`
	Message      string `json:"message"`
}

type runCancelledPayload struct {
	Reason string `json:"reason"`
}

type terminalPayloadError struct {
	kind string
	err  error
}

func (e terminalPayloadError) Error() string {
	return e.err.Error()
}

func (e terminalPayloadError) Unwrap() error {
	return e.err
}

func terminalPayload(kind string, err error) error {
	return terminalPayloadError{kind: kind, err: err}
}

func terminalPayloadFailure(err error) (payloadFailure, bool) {
	var terminal terminalPayloadError
	if !errors.As(err, &terminal) {
		return payloadFailure{}, false
	}
	return payloadFailure{kind: terminal.kind, message: terminal.err.Error()}, true
}

func (s *Server) failLeasedRunPayload(ctx context.Context, row db.LeaseRunExecutionRow, lease dispatch.Lease, failure payloadFailure) error {
	kind, payload, err := payloadFailureRunEvent(failure)
	if err != nil {
		return err
	}
	_, err = s.db.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                row.OrgID,
		RunID:                row.ID,
		ExecutionID:          row.ExecutionID,
		WorkerInstanceID:     row.ExecutionWorkerInstanceID,
		DispatchMessageID:    row.ExecutionDispatchMessageID,
		DispatchLeaseID:      row.ExecutionDispatchLeaseID,
		Status:               db.RunStatusFailed,
		ExitCode:             pgtype.Int4{},
		ErrorMessage:         pgtype.Text{String: failure.message, Valid: true},
		TerminalEventKind:    kind,
		TerminalEventPayload: payload,
	})
	if err != nil {
		return err
	}
	s.ackWorkerQueueLease(ctx, row.ID, lease)
	return nil
}

func payloadFailureRunEvent(failure payloadFailure) (string, []byte, error) {
	payload, err := json.Marshal(runFailurePayload{
		FailureKind: failure.kind,
		Detail:      workerMessagePayload{Message: failure.message},
	})
	if err != nil {
		return "", nil, err
	}
	return "run.failed", payload, nil
}

type workerRunLeaseIDs struct {
	orgID          uuid.UUID
	executionID    uuid.UUID
	runID          uuid.UUID
	attemptNumber  int32
	queueMessageID string
	queueLeaseID   string
}

func parseWorkerRunLease(lease api.WorkerRunLease) (workerRunLeaseIDs, error) {
	if strings.TrimSpace(lease.WorkerInstanceID) == "" {
		return workerRunLeaseIDs{}, errors.New("lease.worker_instance_id is required")
	}
	orgID, err := ids.Parse(lease.OrgID)
	if err != nil {
		return workerRunLeaseIDs{}, errors.New("lease.org_id must be a UUID")
	}
	executionID, err := ids.Parse(lease.ID)
	if err != nil {
		return workerRunLeaseIDs{}, errors.New("lease.id must be a UUID")
	}
	runID, err := ids.Parse(lease.RunID)
	if err != nil {
		return workerRunLeaseIDs{}, errors.New("lease.run_id must be a UUID")
	}
	if lease.AttemptNumber <= 0 {
		return workerRunLeaseIDs{}, errors.New("lease.attempt_number must be positive")
	}
	queueMessageID := strings.TrimSpace(lease.DispatchMessageID)
	if queueMessageID == "" {
		return workerRunLeaseIDs{}, errors.New("lease.dispatch_message_id is required")
	}
	queueLeaseID := strings.TrimSpace(lease.DispatchLeaseID)
	if queueLeaseID == "" {
		return workerRunLeaseIDs{}, errors.New("lease.dispatch_lease_id is required")
	}
	return workerRunLeaseIDs{
		orgID:          orgID,
		executionID:    executionID,
		runID:          runID,
		attemptNumber:  lease.AttemptNumber,
		queueMessageID: queueMessageID,
		queueLeaseID:   queueLeaseID,
	}, nil
}

func (s *Server) workerExecutionLease(ctx context.Context, worker workerActor, leaseIDs workerRunLeaseIDs) (db.GetRunExecutionQueueLeaseRow, dispatch.Lease, error) {
	row, err := s.db.GetRunExecutionQueueLease(ctx, db.GetRunExecutionQueueLeaseParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		ExecutionID:      ids.ToPG(leaseIDs.executionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
	})
	if err != nil {
		return db.GetRunExecutionQueueLeaseRow{}, dispatch.Lease{}, err
	}
	if row.DispatchMessageID != leaseIDs.queueMessageID || row.DispatchLeaseID != leaseIDs.queueLeaseID || row.AttemptNumber != leaseIDs.attemptNumber {
		return db.GetRunExecutionQueueLeaseRow{}, dispatch.Lease{}, pgx.ErrNoRows
	}
	lease := dispatch.Lease{
		ID:               row.DispatchLeaseID,
		MessageID:        row.DispatchMessageID,
		WorkerInstanceID: worker.WorkerInstanceID.String(),
		AttemptNumber:    row.DispatchAttempt,
		ExpiresAt:        pgTime(row.LeaseExpiresAt),
		Message: dispatch.Message{
			OrgID:     leaseIDs.orgID.String(),
			RunID:     ids.MustFromPG(row.RunID).String(),
			QueueName: row.QueueName,
		},
	}
	return row, lease, nil
}

func workerInstanceHeartbeatParams(worker workerActor, capabilities api.WorkerCapabilities) db.UpsertWorkerInstanceHeartbeatParams {
	resources := compute.ResourceVector{
		MilliCPU:  capabilities.MaxVCPUs * 1000,
		MemoryMiB: capabilities.MaxMemoryMiB,
		DiskMiB:   capabilities.MaxDiskMiB,
		Slots:     capabilities.ExecutionSlotsAvailable,
	}
	heartbeat, _ := json.Marshal(workerHeartbeatPayload{
		RuntimeID:       capabilities.RuntimeID,
		RuntimeArch:     capabilities.RuntimeArch,
		RuntimeABI:      capabilities.RuntimeABI,
		KernelDigest:    capabilities.KernelDigest,
		InitramfsDigest: capabilities.InitramfsDigest,
		RootfsDigest:    capabilities.RootfsDigest,
		CNIProfile:      capabilities.CNIProfile,
	})
	labels, _ := json.Marshal(capabilities.Labels)
	supportedProtocolVersions, _ := json.Marshal(capabilities.SupportedProtocolVersions)
	return db.UpsertWorkerInstanceHeartbeatParams{
		ID:                        ids.ToPG(worker.WorkerInstanceID),
		WorkerGroupID:             ids.ToPG(worker.WorkerGroupID),
		ResourceID:                worker.ResourceID,
		Region:                    capabilities.Region,
		TotalMilliCpu:             resources.MilliCPU,
		TotalMemoryMib:            resources.MemoryMiB,
		TotalDiskMib:              resources.DiskMiB,
		TotalExecutionSlots:       resources.Slots,
		AvailableMilliCpu:         resources.MilliCPU,
		AvailableMemoryMib:        resources.MemoryMiB,
		AvailableDiskMib:          resources.DiskMiB,
		AvailableExecutionSlots:   resources.Slots,
		Labels:                    labels,
		Heartbeat:                 heartbeat,
		WorkerVersion:             capabilities.WorkerVersion,
		ProtocolVersion:           capabilities.ProtocolVersion,
		SupportedProtocolVersions: supportedProtocolVersions,
		RuntimeID:                 capabilities.RuntimeID,
		RuntimeArch:               capabilities.RuntimeArch,
		RuntimeABI:                capabilities.RuntimeABI,
		KernelDigest:              capabilities.KernelDigest,
		InitramfsDigest:           capabilities.InitramfsDigest,
		RootfsDigest:              capabilities.RootfsDigest,
		CniProfile:                capabilities.CNIProfile,
	}
}

func releaseFields(result api.WorkerReleaseResult) (db.RunStatus, pgtype.Int4, pgtype.Text, error) {
	switch result.Kind {
	case "completed":
		if result.ExitCode == nil {
			return "", pgtype.Int4{}, pgtype.Text{}, errors.New("result.exit_code is required for completed releases")
		}
		status := db.RunStatusSucceeded
		if *result.ExitCode != 0 {
			status = db.RunStatusFailed
		}
		return status, pgtype.Int4{Int32: *result.ExitCode, Valid: true}, pgtype.Text{}, nil
	case "failed":
		message := "worker execution failed"
		if result.Error != nil && *result.Error != "" {
			message = *result.Error
		}
		return db.RunStatusFailed, pgtype.Int4{}, pgtype.Text{String: message, Valid: true}, nil
	case "cancelled":
		message := "worker execution cancelled"
		if result.Error != nil && *result.Error != "" {
			message = *result.Error
		}
		return db.RunStatusCancelled, pgtype.Int4{}, pgtype.Text{String: message, Valid: true}, nil
	default:
		return "", pgtype.Int4{}, pgtype.Text{}, fmt.Errorf("unsupported release result kind %q", result.Kind)
	}
}

func releaseOutput(result api.WorkerReleaseResult, status db.RunStatus, exitCode pgtype.Int4) []byte {
	if status != db.RunStatusSucceeded || !exitCode.Valid || exitCode.Int32 != 0 || len(result.Output) == 0 {
		return nil
	}
	return append([]byte(nil), result.Output...)
}

func terminalRunEventForFields(status db.RunStatus, exitCode pgtype.Int4, errorMessage pgtype.Text, result api.WorkerReleaseResult) (string, []byte, error) {
	switch status {
	case db.RunStatusSucceeded:
		code := int32(0)
		if exitCode.Valid {
			code = exitCode.Int32
		}
		payload, err := json.Marshal(runCompletedPayload{ExitCode: code})
		return "run.completed", payload, err
	case db.RunStatusFailed:
		if exitCode.Valid {
			payload, err := json.Marshal(runFailurePayload{
				FailureKind: "task_failed",
				Detail:      taskFailedDetailPayload{ExitCode: exitCode.Int32},
			})
			return "run.failed", payload, err
		}
		message := "worker execution failed"
		if errorMessage.Valid && strings.TrimSpace(errorMessage.String) != "" {
			message = errorMessage.String
		}
		if failureKind, ok := trustedWorkerFailureKind(result); ok {
			payload, err := json.Marshal(runFailurePayload{
				FailureKind: failureKind,
				Detail: workerFailureDetailPayload{
					Message:      message,
					LimitSeconds: result.LimitSeconds,
				},
			})
			return "run.failed", payload, err
		}
		payload, err := json.Marshal(runFailurePayload{
			FailureKind: "worker_failed",
			Detail:      workerMessagePayload{Message: message},
		})
		return "run.failed", payload, err
	case db.RunStatusCancelled:
		reason := "worker execution cancelled"
		if errorMessage.Valid && strings.TrimSpace(errorMessage.String) != "" {
			reason = errorMessage.String
		}
		payload, err := json.Marshal(runCancelledPayload{Reason: reason})
		return "run.cancelled", payload, err
	default:
		return "", nil, fmt.Errorf("run status %q is not terminal", status)
	}
}

func trustedWorkerFailureKind(result api.WorkerReleaseResult) (string, bool) {
	if result.FailureKind == nil {
		return "", false
	}
	switch *result.FailureKind {
	case "max_duration", "task_not_found", "duplicate_task_id", "missing_config", "task_parse_failed":
		return *result.FailureKind, true
	default:
		return "", false
	}
}

func workerRunLeaseResponse(row db.LeaseRunExecutionRow) api.WorkerRunLease {
	return api.WorkerRunLease{
		ID:                ids.MustFromPG(row.ExecutionID).String(),
		OrgID:             ids.MustFromPG(row.OrgID).String(),
		RunID:             ids.MustFromPG(row.ID).String(),
		WorkerInstanceID:  ids.MustFromPG(row.ExecutionWorkerInstanceID).String(),
		ProtocolVersion:   row.ExecutionWorkerProtocolVersion,
		AttemptNumber:     row.ExecutionAttemptNumber,
		DispatchMessageID: row.ExecutionDispatchMessageID,
		DispatchLeaseID:   row.ExecutionDispatchLeaseID,
		ExpiresAt:         pgTime(row.ExecutionLeaseExpiresAt),
	}
}

func (s *Server) workerRunFromLease(ctx context.Context, row db.LeaseRunExecutionRow) (api.WorkerRun, error) {
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
		resolvedSecrets, err = s.secrets.ResolveScopedNames(ctx, ids.MustFromPG(row.OrgID), ids.MustFromPG(row.ProjectID), ids.MustFromPG(row.EnvironmentID), secretNames)
		if err != nil {
			if secret.IsUnavailable(err) || errors.Is(err, pgx.ErrNoRows) {
				return api.WorkerRun{}, terminalPayload("secret_unavailable", err)
			}
			return api.WorkerRun{}, err
		}
	}
	requirements, err := workerRunRequirementsFromLease(row)
	if err != nil {
		return api.WorkerRun{}, err
	}
	run := api.WorkerRun{
		ID:                    ids.MustFromPG(row.ID).String(),
		Version:               row.RunDeploymentVersion,
		DeploymentVersion:     row.RunDeploymentVersion,
		APIVersion:            row.RunApiVersion,
		SDKVersion:            row.RunSdkVersion,
		CLIVersion:            row.RunCliVersion,
		WorkerProtocolVersion: row.ExecutionWorkerProtocolVersion,
		AttemptNumber:         row.ExecutionAttemptNumber,
		TaskID:                row.TaskID,
		Payload:               json.RawMessage(row.Payload),
		Secrets:               resolvedSecrets,
		DeploymentSource: api.DeploymentSourceArtifact{
			Digest:    row.DeploymentSourceDigest,
			MediaType: api.DeploymentSourceArtifactMediaType,
		},
		DeploymentTask: api.WorkerDeploymentTask{
			ID:                  ids.MustFromPG(row.DeploymentTaskID).String(),
			FilePath:            row.DeploymentTaskFilePath,
			ExportName:          row.DeploymentTaskExportName,
			HandlerEntrypoint:   row.DeploymentTaskHandlerEntrypoint,
			BundleDigest:        row.DeploymentTaskBundleDigest,
			BundleFormatVersion: row.DeploymentTaskBundleFormatVersion,
		},
		Requirements:       requirements,
		MaxDurationSeconds: row.MaxDurationSeconds,
		ActiveDurationMs:   row.ActiveDurationMs,
		Restore:            restore,
	}
	return run, nil
}

func workerRunRequirementsFromLease(row db.LeaseRunExecutionRow) (compute.RunRuntimeRequirements, error) {
	network := compute.DefaultNetworkPolicy()
	if len(row.RequirementsNetworkPolicy) > 0 {
		if err := json.Unmarshal(row.RequirementsNetworkPolicy, &network); err != nil {
			return compute.RunRuntimeRequirements{}, fmt.Errorf("worker run network policy: %w", err)
		}
	}
	var placement compute.Placement
	if len(row.RequirementsPlacement) > 0 {
		if err := json.Unmarshal(row.RequirementsPlacement, &placement); err != nil {
			return compute.RunRuntimeRequirements{}, fmt.Errorf("worker run placement: %w", err)
		}
	}
	requirements := compute.RunRuntimeRequirements{
		Resources: compute.ResourceVector{
			MilliCPU:  row.RequestedMilliCpu,
			MemoryMiB: row.RequestedMemoryMib,
			DiskMiB:   row.RequestedDiskMib,
			Slots:     row.RequestedExecutionSlots,
		},
		Runtime: compute.RuntimeSelector{
			ID:              row.RequirementsRuntimeID,
			Arch:            row.RequirementsRuntimeArch,
			ABI:             row.RequirementsRuntimeAbi,
			KernelDigest:    row.RequirementsKernelDigest,
			InitramfsDigest: row.RequirementsInitramfsDigest,
			RootfsDigest:    row.RequirementsRootfsDigest,
			CNIProfile:      row.RequirementsCniProfile,
		},
		Network:   network,
		Placement: placement,
	}
	return requirements, requirements.Validate()
}

func normalizeWorkerCapabilities(input api.WorkerCapabilities) (api.WorkerCapabilities, error) {
	capabilities := api.WorkerCapabilities{
		ProtocolVersion:           strings.TrimSpace(input.ProtocolVersion),
		SupportedProtocolVersions: normalizeWorkerProtocolVersions(input.SupportedProtocolVersions),
		WorkerVersion:             strings.TrimSpace(input.WorkerVersion),
		RuntimeID:                 strings.TrimSpace(input.RuntimeID),
		RuntimeArch:               strings.TrimSpace(input.RuntimeArch),
		RuntimeABI:                strings.TrimSpace(input.RuntimeABI),
		KernelDigest:              strings.TrimSpace(input.KernelDigest),
		InitramfsDigest:           strings.TrimSpace(input.InitramfsDigest),
		RootfsDigest:              strings.TrimSpace(input.RootfsDigest),
		CNIProfile:                strings.TrimSpace(input.CNIProfile),
		Region:                    strings.TrimSpace(input.Region),
		MaxVCPUs:                  input.MaxVCPUs,
		MaxMemoryMiB:              input.MaxMemoryMiB,
		MaxDiskMiB:                input.MaxDiskMiB,
		ExecutionSlotsAvailable:   input.ExecutionSlotsAvailable,
		Network: api.WorkerNetworkCapabilities{
			Internet:      input.Network.Internet,
			BlockInternet: input.Network.BlockInternet,
			DenyCIDRs:     input.Network.DenyCIDRs,
			AllowCIDRs:    input.Network.AllowCIDRs,
			AllowDomains:  input.Network.AllowDomains,
		},
	}
	labels, err := normalizeWorkerLabels(input.Labels)
	if err != nil {
		return api.WorkerCapabilities{}, err
	}
	capabilities.Labels = labels
	if capabilities.ProtocolVersion == "" {
		return api.WorkerCapabilities{}, errors.New("worker protocol_version is required")
	}
	if capabilities.ProtocolVersion != api.CurrentWorkerProtocolVersion {
		return api.WorkerCapabilities{}, fmt.Errorf("worker protocol_version %s is not supported; current protocol is %s", capabilities.ProtocolVersion, api.CurrentWorkerProtocolVersion)
	}
	if !slices.Contains(capabilities.SupportedProtocolVersions, api.CurrentWorkerProtocolVersion) {
		return api.WorkerCapabilities{}, fmt.Errorf("worker supported_protocol_versions must include %s", api.CurrentWorkerProtocolVersion)
	}
	if capabilities.RuntimeID == "" {
		return api.WorkerCapabilities{}, errors.New("worker runtime_id is required")
	}
	if capabilities.RuntimeArch == "" {
		return api.WorkerCapabilities{}, errors.New("worker runtime_arch is required")
	}
	if capabilities.RuntimeABI == "" {
		return api.WorkerCapabilities{}, errors.New("worker runtime_abi is required")
	}
	if capabilities.KernelDigest == "" {
		return api.WorkerCapabilities{}, errors.New("worker kernel_digest is required")
	}
	if capabilities.InitramfsDigest == "" {
		return api.WorkerCapabilities{}, errors.New("worker initramfs_digest is required")
	}
	if capabilities.RootfsDigest == "" {
		return api.WorkerCapabilities{}, errors.New("worker rootfs_digest is required")
	}
	if capabilities.CNIProfile == "" {
		return api.WorkerCapabilities{}, errors.New("worker cni_profile is required")
	}
	expectedRuntimeID, err := compute.RuntimeIdentityDigest(compute.RuntimeSelector{
		Arch:            capabilities.RuntimeArch,
		ABI:             capabilities.RuntimeABI,
		KernelDigest:    capabilities.KernelDigest,
		InitramfsDigest: capabilities.InitramfsDigest,
		RootfsDigest:    capabilities.RootfsDigest,
		CNIProfile:      capabilities.CNIProfile,
	})
	if err != nil {
		return api.WorkerCapabilities{}, fmt.Errorf("worker runtime identity: %w", err)
	}
	if capabilities.RuntimeID != expectedRuntimeID {
		return api.WorkerCapabilities{}, fmt.Errorf("worker runtime_id %s does not match runtime identity %s", capabilities.RuntimeID, expectedRuntimeID)
	}
	if capabilities.MaxVCPUs <= 0 {
		return api.WorkerCapabilities{}, errors.New("worker max_vcpus must be positive")
	}
	if capabilities.MaxVCPUs > math.MaxInt32 {
		return api.WorkerCapabilities{}, fmt.Errorf("worker max_vcpus exceeds max %d", math.MaxInt32)
	}
	if capabilities.MaxMemoryMiB <= 0 {
		return api.WorkerCapabilities{}, errors.New("worker max_memory_mib must be positive")
	}
	if capabilities.MaxMemoryMiB > math.MaxInt32 {
		return api.WorkerCapabilities{}, fmt.Errorf("worker max_memory_mib exceeds max %d", math.MaxInt32)
	}
	if capabilities.MaxDiskMiB <= 0 {
		return api.WorkerCapabilities{}, errors.New("worker max_disk_mib must be positive")
	}
	if capabilities.MaxDiskMiB > math.MaxInt32 {
		return api.WorkerCapabilities{}, fmt.Errorf("worker max_disk_mib exceeds max %d", math.MaxInt32)
	}
	if capabilities.ExecutionSlotsAvailable <= 0 {
		return api.WorkerCapabilities{}, errors.New("worker execution_slots_available must be positive")
	}
	if !capabilities.Network.Internet {
		return api.WorkerCapabilities{}, errors.New("worker network.internet capability is required")
	}
	if !capabilities.Network.BlockInternet {
		return api.WorkerCapabilities{}, errors.New("worker network.block_internet capability is required")
	}
	if !capabilities.Network.DenyCIDRs {
		return api.WorkerCapabilities{}, errors.New("worker network.deny_cidrs capability is required")
	}
	return capabilities, nil
}

func normalizeWorkerProtocolVersions(input []string) []string {
	versions := make([]string, 0, len(input))
	seen := map[string]struct{}{}
	for _, raw := range input {
		version := strings.TrimSpace(raw)
		if version == "" {
			continue
		}
		if _, ok := seen[version]; ok {
			continue
		}
		seen[version] = struct{}{}
		versions = append(versions, version)
	}
	return versions
}

func firstPositiveInt32(values ...int32) int32 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func normalizeWorkerLabels(input map[string]string) (map[string]string, error) {
	labels := make(map[string]string, len(input))
	for key, value := range input {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return nil, errors.New("worker label key is required")
		}
		labels[key] = value
	}
	return labels, nil
}

func (s *Server) workerRestorePayload(ctx context.Context, row db.LeaseRunExecutionRow) (*api.WorkerRestore, error) {
	payload, err := s.db.GetRunRestorePayload(ctx, db.GetRunRestorePayloadParams{
		OrgID:            row.OrgID,
		RunID:            row.ID,
		ExecutionID:      row.ExecutionID,
		WorkerInstanceID: row.ExecutionWorkerInstanceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		if row.ExecutionRestoreCheckpointID.Valid {
			return nil, fmt.Errorf("restore checkpoint %s is unavailable", ids.MustFromPG(row.ExecutionRestoreCheckpointID).String())
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var manifest api.WorkerCheckpointManifest
	if err := json.Unmarshal(payload.Manifest, &manifest); err != nil {
		return nil, fmt.Errorf("decode checkpoint manifest: %w", err)
	}
	return &api.WorkerRestore{
		CheckpointID: ids.MustFromPG(payload.CheckpointID).String(),
		Checkpoint:   manifest,
		Waitpoint: api.WorkerRestoreWaitpoint{
			ID:                ids.MustFromPG(payload.WaitpointID).String(),
			RunWaitID:         ids.MustFromPG(payload.RunWaitID).String(),
			Kind:              string(payload.WaitpointKind),
			ResumeKind:        payload.ResolutionKind.String,
			ResumePayloadJSON: json.RawMessage(payload.Resolution),
		},
	}, nil
}
