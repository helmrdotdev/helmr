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
	buildStore := s.db
	commit := func() error { return nil }
	rollback := func() {}
	if s.tx != nil {
		tx, err := s.tx.Begin(r.Context())
		if err != nil {
			writeError(w, errors.New("begin deployment build lease"))
			return
		}
		defer tx.Rollback(r.Context())
		buildStore = db.New(tx)
		commit = func() error { return tx.Commit(r.Context()) }
		rollback = func() {}
	}
	row, err := buildStore.LeaseQueuedDeploymentBuild(r.Context(), db.LeaseQueuedDeploymentBuildParams{
		WorkerGroupID:         pgvalue.UUID(worker.WorkerGroupID),
		BuildLeaseID:          pgtype.Text{String: leaseID, Valid: true},
		BuildWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		BuildLeaseExpiresAt:   pgtype.Timestamptz{Time: leaseExpiresAt, Valid: true},
	})
	if isNoRows(err) {
		writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildLeaseResponse{})
		return
	}
	if err != nil {
		s.log.Error("worker deployment build lease failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("lease deployment build"))
		return
	}
	if err := appendDeploymentLifecycleEvent(r.Context(), buildStore, row.OrgID, row.ProjectID, row.EnvironmentID, row.ID, "deployment.building", "info", "worker", "building", "Deployment build started"); err != nil {
		rollback()
		s.log.Error("record deployment building event failed", "deployment_id", pgvalue.MustUUIDValue(row.ID).String(), "error", err)
		writeError(w, errors.New("record deployment event"))
		return
	}
	if err := commit(); err != nil {
		s.log.Error("commit deployment build lease failed", "deployment_id", pgvalue.MustUUIDValue(row.ID).String(), "error", err)
		writeError(w, errors.New("commit deployment build lease"))
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
	failBuild := func(message string) bool {
		payload, err := json.Marshal(workerMessagePayload{Message: strings.TrimSpace(message)})
		if err != nil {
			writeError(w, errors.New("marshal deployment build error"))
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
		if isNoRows(err) {
			writeError(w, conflict(errors.New("deployment build lease is stale")))
			return false
		}
		if err != nil {
			writeError(w, errors.New("mark deployment build failed"))
			return false
		}
		if err := appendDeploymentLifecycleEvent(r.Context(), queries, row.OrgID, row.ProjectID, row.EnvironmentID, row.ID, "deployment.failed", "error", "worker", "failed", strings.TrimSpace(message)); err != nil {
			writeError(w, errors.New("record deployment event"))
			return false
		}
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, errors.New("commit deployment build failure"))
			return false
		}
		writeJSON(w, http.StatusOK, api.WorkerDeploymentBuildResponse{DeploymentID: pgvalue.MustUUIDValue(row.ID).String(), Status: string(row.Status)})
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
	}); isNoRows(err) {
		writeError(w, conflict(errors.New("deployment build lease is stale")))
		return
	} else if err != nil {
		writeError(w, errors.New("get deployment build lease"))
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
			QueueConcurrencyLimit: pgvalue.Int4Ptr(task.ConcurrencyLimit),
			Ttl:                   strings.TrimSpace(task.TTL),
			MaxDurationSeconds:    task.MaxDurationSeconds,
			RetryPolicy:           retryPolicy,
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
