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
	capabilities, err := normalizeWorkerCapabilities(request.Capabilities)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if _, err := s.db.UpsertWorkerInstanceHeartbeat(r.Context(), workerInstanceHeartbeatParams(worker, capabilities)); err != nil {
		s.log.Error("worker heartbeat failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("record worker heartbeat"))
		return
	}
	if err := s.db.EnsureRuntimeReleaseSelection(r.Context(), capabilities.RuntimeID); err != nil {
		s.log.Error("ensure runtime release selection failed", "worker_instance_id", worker.WorkerInstanceID.String(), "runtime_id", capabilities.RuntimeID, "error", err)
		writeError(w, errors.New("select runtime release"))
		return
	}
	capacity, err := s.db.GetWorkerInstanceQueueCapacity(r.Context(), pgvalue.UUID(worker.WorkerInstanceID))
	if isNoRows(err) {
		writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildLeaseResponse{})
		return
	}
	if err != nil {
		s.log.Error("worker capacity lookup failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("get worker capacity"))
		return
	}
	if capacity.AvailableExecutionSlots <= 0 || capacity.AvailableMilliCpu <= 0 || capacity.AvailableMemoryMib <= 0 {
		writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildLeaseResponse{})
		return
	}
	leaseID := uuid.Must(uuid.NewV7()).String()
	leaseExpiresAt := time.Now().Add(deploymentBuildLeaseDuration)
	var row db.LeaseQueuedDeploymentBuildRow
	leased := false
	err = s.inTx(r.Context(), func(work *txWork) error {
		var err error
		row, err = work.q.LeaseQueuedDeploymentBuild(r.Context(), db.LeaseQueuedDeploymentBuildParams{
			WorkerGroupID:         pgvalue.UUID(worker.WorkerGroupID),
			BuildLeaseID:          pgtype.Text{String: leaseID, Valid: true},
			BuildWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			BuildLeaseExpiresAt:   pgtype.Timestamptz{Time: leaseExpiresAt, Valid: true},
		})
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			s.log.Error("worker deployment build lease failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
			return errors.New("lease deployment build")
		}
		if err := appendDeploymentLifecycleEvent(r.Context(), work.q, row.OrgID, row.ProjectID, row.EnvironmentID, row.ID, "deployment.building", "info", "worker", "building", "Deployment build started"); err != nil {
			s.log.Error("record deployment building event failed", "deployment_id", pgvalue.MustUUIDValue(row.ID).String(), "error", err)
			return errors.New("record deployment event")
		}
		leased = true
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if !leased {
		writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildLeaseResponse{})
		return
	}
	deploymentID := pgvalue.MustUUIDValue(row.ID).String()
	lease := api.WorkerDeploymentBuildLease{
		ID:               leaseID,
		OrgID:            pgvalue.MustUUIDValue(row.OrgID).String(),
		ProjectID:        pgvalue.MustUUIDValue(row.ProjectID).String(),
		EnvironmentID:    pgvalue.MustUUIDValue(row.EnvironmentID).String(),
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

func (s *Server) workerCompleteDeploymentBuild(w http.ResponseWriter, r *http.Request) {
	worker := workerFromContext(r.Context())
	if s.db == nil || s.tx == nil {
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
	if time.Now().After(request.Lease.ExpiresAt) {
		writeError(w, conflict(errors.New("deployment build lease expired")))
		return
	}
	orgID, projectID, environmentID, deploymentID, err := parseDeploymentBuildLeaseIDs(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}

	tx, err := s.tx.Begin(r.Context())
	if err != nil {
		writeError(w, errors.New("begin deployment build completion"))
		return
	}
	defer tx.Rollback(r.Context())
	queries := db.New(tx)
	buildWorkerInstanceID := pgvalue.UUID(worker.WorkerInstanceID)
	failBuild := func(message string) {
		payload, err := json.Marshal(workerMessagePayload{Message: strings.TrimSpace(message)})
		if err != nil {
			writeError(w, errors.New("marshal deployment build error"))
			return
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
		if isNoRows(err) {
			writeError(w, conflict(errors.New("deployment build lease is stale")))
			return
		}
		if err != nil {
			writeError(w, errors.New("mark deployment build failed"))
			return
		}
		if err := appendDeploymentLifecycleEvent(r.Context(), queries, row.OrgID, row.ProjectID, row.EnvironmentID, row.ID, "deployment.failed", "error", "worker", "failed", strings.TrimSpace(message)); err != nil {
			writeError(w, errors.New("record deployment event"))
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, errors.New("commit deployment build failure"))
			return
		}
		writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildResponse{DeploymentID: pgvalue.MustUUIDValue(row.ID).String(), Status: string(row.Status)})
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
	}); isNoRows(err) {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	} else if err != nil {
		writeError(w, errors.New("get deployment build lease"))
		return
	}
	workerState, err := queries.GetWorkerInstanceState(r.Context(), buildWorkerInstanceID)
	if isNoRows(err) {
		failBuild("deployment build worker instance was not found")
		return
	}
	if err != nil {
		writeError(w, errors.New("get deployment build worker state"))
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
	queueConcurrencyLimits := map[string]*int32{}
	for _, queue := range request.Result.Queues {
		queueName := strings.TrimSpace(queue.Name)
		queueConcurrencyLimits[queueName] = queue.ConcurrencyLimit
		if _, err := queries.CreateDeploymentQueue(r.Context(), db.CreateDeploymentQueueParams{
			ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:            orgID,
			ProjectID:        projectID,
			EnvironmentID:    environmentID,
			DeploymentID:     deploymentID,
			Name:             queueName,
			ConcurrencyLimit: pgvalue.Int4Ptr(queue.ConcurrencyLimit),
		}); err != nil {
			failBuild("record deployment queue: " + err.Error())
			return
		}
	}
	deploymentSandboxIDs := map[string]pgtype.UUID{}
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
		sandboxID := strings.TrimSpace(task.SandboxID)
		deploymentSandboxID, ok := deploymentSandboxIDs[sandboxID]
		if !ok {
			imageArtifact, err := createDeploymentBuildArtifact(r.Context(), queries, orgID, projectID, environmentID, buildWorkerInstanceID, strings.TrimSpace(task.SandboxImageArtifact.Digest), db.ArtifactKindSandboxImage, casObjectByDigest)
			if err != nil {
				failBuild("record deployment sandbox image artifact: " + err.Error())
				return
			}
			resourceFloor, err := json.Marshal(map[string]any{
				"milli_cpu":  task.RequestedMilliCPU,
				"memory_mib": task.RequestedMemoryMiB,
				"disk_mib":   task.RequestedDiskMiB,
			})
			if err != nil {
				failBuild("encode deployment sandbox resource floor: " + err.Error())
				return
			}
			fingerprint, err := deploymentSandboxContractFingerprint(deploymentSandboxContractFingerprintInput{
				RootfsDigest:       workerState.RootfsDigest,
				RuntimeABI:         workerState.RuntimeABI,
				GuestdABI:          currentGuestdABI,
				AdapterABI:         currentAdapterABI,
				WorkspaceMountPath: strings.TrimSpace(task.WorkspaceMountPath),
				NetworkPolicy:      task.Network,
				FilesystemFormat:   strings.TrimSpace(task.FilesystemFormat),
				ContractVersion:    1,
			})
			if err != nil {
				failBuild("fingerprint deployment sandbox contract: " + err.Error())
				return
			}
			row, err := queries.CreateDeploymentSandbox(r.Context(), db.CreateDeploymentSandboxParams{
				ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:               orgID,
				ProjectID:           projectID,
				EnvironmentID:       environmentID,
				DeploymentID:        deploymentID,
				SandboxID:           sandboxID,
				ImageArtifactID:     imageArtifact.ID,
				ImageArtifactFormat: strings.TrimSpace(task.SandboxImageArtifactFormat),
				RootfsDigest:        workerState.RootfsDigest,
				ImageDigest:         imageArtifact.Digest,
				ImageFormat:         strings.TrimSpace(task.SandboxImageFormat),
				WorkspaceMountPath:  strings.TrimSpace(task.WorkspaceMountPath),
				ResourceFloor:       resourceFloor,
				DiskFloorMib:        task.RequestedDiskMiB,
				NetworkPolicy:       networkPolicy,
				RuntimeABI:          workerState.RuntimeABI,
				GuestdAbi:           currentGuestdABI,
				AdapterAbi:          currentAdapterABI,
				FilesystemFormat:    strings.TrimSpace(task.FilesystemFormat),
				DefaultUid:          pgtype.Int8{},
				DefaultGid:          pgtype.Int8{},
				DefaultWorkdir:      "",
				ContractVersion:     1,
				Fingerprint:         fingerprint,
			})
			if err != nil {
				failBuild("record deployment sandbox: " + err.Error())
				return
			}
			deploymentSandboxID = row.ID
			deploymentSandboxIDs[sandboxID] = deploymentSandboxID
		}
		queueName := strings.TrimSpace(task.QueueName)
		queueConcurrencyLimit, ok := queueConcurrencyLimits[queueName]
		if !ok {
			failBuild("deployment task references undefined queue")
			return
		}
		retryPolicy, err := normalizedRetryPolicy(task.RetryPolicy)
		if err != nil {
			failBuild("validate deployment task retry policy: " + err.Error())
			return
		}
		if _, err := queries.CreateDeploymentTask(r.Context(), db.CreateDeploymentTaskParams{
			ID:                    pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:                 orgID,
			ProjectID:             projectID,
			EnvironmentID:         environmentID,
			DeploymentID:          deploymentID,
			DeploymentSandboxID:   deploymentSandboxID,
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
			QueueName:             queueName,
			QueueConcurrencyLimit: pgvalue.Int4Ptr(queueConcurrencyLimit),
			Ttl:                   strings.TrimSpace(task.TTL),
			MaxActiveDurationMs:   int64(task.MaxDurationSeconds) * 1000,
			RetryPolicy:           retryPolicy,
		}); err != nil {
			failBuild("record deployment task: " + err.Error())
			return
		}
	}
	for _, stream := range request.Result.Streams {
		schemaJSON := stream.SchemaJSON
		if len(schemaJSON) == 0 {
			schemaJSON = []byte("null")
		}
		if _, err := queries.UpsertDeploymentStream(r.Context(), db.UpsertDeploymentStreamParams{
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
			failBuild("record deployment stream: " + err.Error())
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
	if isNoRows(err) {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("mark deployment deployed"))
		return
	}
	if err := appendDeploymentLifecycleEvent(r.Context(), queries, row.OrgID, row.ProjectID, row.EnvironmentID, row.ID, "deployment.deployed", "info", "worker", "deployed", "Deployment build completed"); err != nil {
		writeError(w, errors.New("record deployment event"))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, errors.New("commit deployment build completion"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildResponse{DeploymentID: pgvalue.MustUUIDValue(row.ID).String(), Status: string(row.Status)})
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

func createDeploymentBuildArtifact(ctx context.Context, queries *db.Queries, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, workerInstanceID pgtype.UUID, digest string, kind db.ArtifactKind, objects map[string]api.CASObject) (db.Artifact, error) {
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
