package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5/pgtype"
)

type deploymentStore interface {
	AllocateDeploymentVersion(context.Context, db.AllocateDeploymentVersionParams) (string, error)
	AppendDeploymentEvent(context.Context, db.AppendDeploymentEventParams) (db.AppendDeploymentEventRow, error)
	CreateArtifact(context.Context, db.CreateArtifactParams) (db.Artifact, error)
	CreateDeployment(context.Context, db.CreateDeploymentParams) (db.Deployment, error)
	GetReusableDeploymentByContentHash(context.Context, db.GetReusableDeploymentByContentHashParams) (db.Deployment, error)
	ListArtifactsByIDs(context.Context, db.ListArtifactsByIDsParams) ([]db.Artifact, error)
	LockDeploymentReusableBuildKey(context.Context, db.LockDeploymentReusableBuildKeyParams) error
	UpsertCasObject(context.Context, db.UpsertCasObjectParams) (db.CasObject, error)
}

type currentDeploymentStore interface {
	GetCurrentDeployment(context.Context, db.GetCurrentDeploymentParams) (db.Deployment, error)
	ListArtifactsByIDs(context.Context, db.ListArtifactsByIDsParams) ([]db.Artifact, error)
	ListDeploymentTasks(context.Context, db.ListDeploymentTasksParams) ([]db.DeploymentTask, error)
}

type deploymentStatusStore interface {
	GetDeployment(context.Context, db.GetDeploymentParams) (db.Deployment, error)
	GetDeploymentForOrg(context.Context, db.GetDeploymentForOrgParams) (db.Deployment, error)
	GetDeploymentByVersion(context.Context, db.GetDeploymentByVersionParams) (db.Deployment, error)
	ListArtifactsByIDs(context.Context, db.ListArtifactsByIDsParams) ([]db.Artifact, error)
	ListScopedDeployments(context.Context, db.ListScopedDeploymentsParams) ([]db.Deployment, error)
	ListDeploymentsByVersionForOrg(context.Context, db.ListDeploymentsByVersionForOrgParams) ([]db.Deployment, error)
	ListDeploymentTasks(context.Context, db.ListDeploymentTasksParams) ([]db.DeploymentTask, error)
	PromoteDeployment(context.Context, db.PromoteDeploymentParams) (db.PromoteDeploymentRow, error)
}

type currentDeploymentReadStore interface {
	ListArtifactsByIDs(context.Context, db.ListArtifactsByIDsParams) ([]db.Artifact, error)
	ListCurrentDeploymentTasks(context.Context, db.ListCurrentDeploymentTasksParams) ([]db.DeploymentTask, error)
	GetCurrentDeploymentTask(context.Context, db.GetCurrentDeploymentTaskParams) (db.GetCurrentDeploymentTaskRow, error)
	ListCurrentDeploymentSandboxes(context.Context, db.ListCurrentDeploymentSandboxesParams) ([]db.DeploymentSandbox, error)
	GetCurrentDeploymentSandbox(context.Context, db.GetCurrentDeploymentSandboxParams) (db.DeploymentSandbox, error)
}

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	store, ok := s.db.(deploymentStatusStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) && !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		writeError(w, errors.New("list deployments"))
		return
	}
	limit := int32(50)
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed <= 0 || parsed > 200 {
			writeError(w, badRequest(errors.New("limit must be an integer between 1 and 200")))
			return
		}
		limit = int32(parsed)
	}
	rows, err := store.ListScopedDeployments(r.Context(), db.ListScopedDeploymentsParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		RowLimit:      limit,
	})
	if err != nil {
		s.log.Error("list deployments failed", "error", err)
		writeError(w, errors.New("list deployments"))
		return
	}
	response := make([]api.DeploymentResponse, 0, len(rows))
	for _, row := range rows {
		item, err := deploymentResponseWithArtifacts(r.Context(), store, row)
		if err != nil {
			s.log.Error("get deployment artifacts failed", "deployment_id", pgvalue.MustUUIDValue(row.ID).String(), "error", err)
			writeError(w, errors.New("list deployments"))
			return
		}
		item.Tasks = []api.DeploymentTaskResponse{}
		response = append(response, item)
	}
	writeJSON(w, http.StatusOK, api.ListDeploymentsResponse{Deployments: response})
}

func (s *Server) loadCurrentDeploymentReadScope(w http.ResponseWriter, r *http.Request) (currentDeploymentReadStore, auth.Scope, pgtype.UUID, pgtype.UUID, bool) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return nil, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	store, ok := s.db.(currentDeploymentReadStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return nil, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return nil, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		writeError(w, errors.New("resolve current deployment read scope"))
		return nil, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	return store, scope, projectID, environmentID, true
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	store, ok := s.db.(deploymentStatusStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	deploymentID, err := parseUUIDParam(r, "deploymentID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) && !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		s.log.Error("resolve deployment scope failed", "error", err)
		writeError(w, errors.New("get deployment"))
		return
	}
	deployment, err := store.GetDeploymentForOrg(r.Context(), db.GetDeploymentForOrgParams{
		OrgID: pgvalue.UUID(actor.OrgID),
		ID:    pgvalue.UUID(deploymentID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("deployment not found")))
		return
	}
	if err != nil {
		s.log.Error("get deployment failed", "deployment_id", deploymentID.String(), "error", err)
		writeError(w, errors.New("get deployment"))
		return
	}
	if deployment.ProjectID != projectID || deployment.EnvironmentID != environmentID {
		writeError(w, notFound(errors.New("deployment not found")))
		return
	}
	if _, err := s.requireEnvironmentPlacementWorkerGroup(r.Context(), s.db, actor.OrgID, deployment.ProjectID, deployment.EnvironmentID); err != nil {
		writeError(w, err)
		return
	}
	response, err := deploymentResponseWithArtifacts(r.Context(), store, deployment)
	if err != nil {
		s.log.Error("get deployment artifacts failed", "deployment_id", deploymentID.String(), "error", err)
		writeError(w, errors.New("get deployment"))
		return
	}
	response.Tasks = []api.DeploymentTaskResponse{}
	if deployment.Status == db.DeploymentStatusDeployed {
		tasks, err := store.ListDeploymentTasks(r.Context(), db.ListDeploymentTasksParams{
			OrgID:         pgvalue.UUID(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			DeploymentID:  deployment.ID,
		})
		if err != nil {
			s.log.Error("list deployment tasks failed", "deployment_id", deploymentID.String(), "error", err)
			writeError(w, errors.New("list deployment tasks"))
			return
		}
		response.Tasks = make([]api.DeploymentTaskResponse, 0, len(tasks))
		taskResponses, err := deploymentTaskResponses(r.Context(), store, tasks)
		if err != nil {
			s.log.Error("get deployment task artifacts failed", "deployment_id", deploymentID.String(), "error", err)
			writeError(w, errors.New("list deployment tasks"))
			return
		}
		response.Tasks = taskResponses
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getCurrentDeployment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	store, ok := s.db.(currentDeploymentStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		s.log.Error("resolve deployment scope failed", "error", err)
		writeError(w, errors.New("get current deployment"))
		return
	}
	deployment, err := store.GetCurrentDeployment(r.Context(), db.GetCurrentDeploymentParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
	})
	if isNoRows(err) {
		writeJSON(w, http.StatusOK, api.GetCurrentDeploymentResponse{})
		return
	}
	if err != nil {
		s.log.Error("get current deployment failed", "error", err)
		writeError(w, errors.New("get current deployment"))
		return
	}
	tasks, err := store.ListDeploymentTasks(r.Context(), db.ListDeploymentTasksParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		DeploymentID:  deployment.ID,
	})
	if err != nil {
		s.log.Error("list deployment tasks failed", "deployment_id", pgvalue.MustUUIDValue(deployment.ID).String(), "error", err)
		writeError(w, errors.New("list deployment tasks"))
		return
	}
	response, err := deploymentResponseWithArtifacts(r.Context(), store, deployment)
	if err != nil {
		s.log.Error("get current deployment artifacts failed", "deployment_id", pgvalue.MustUUIDValue(deployment.ID).String(), "error", err)
		writeError(w, errors.New("get current deployment"))
		return
	}
	response.Tasks = make([]api.DeploymentTaskResponse, 0, len(tasks))
	taskResponses, err := deploymentTaskResponses(r.Context(), store, tasks)
	if err != nil {
		s.log.Error("get current deployment task artifacts failed", "deployment_id", pgvalue.MustUUIDValue(deployment.ID).String(), "error", err)
		writeError(w, errors.New("list deployment tasks"))
		return
	}
	response.Tasks = taskResponses
	writeJSON(w, http.StatusOK, api.GetCurrentDeploymentResponse{Deployment: &response})
}

func (s *Server) promoteDeployment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	store, ok := s.db.(deploymentStatusStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	var request api.PromoteDeploymentRequest
	if err := decodeOptionalJSON(r.Body, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid promotion request JSON: %w", err)))
		return
	}
	actor := actorFromContext(r.Context())
	deploymentRef := strings.TrimSpace(chi.URLParam(r, "deployment"))
	if deploymentRef == "" {
		writeError(w, badRequest(errors.New("deployment is required")))
		return
	}
	projectRef, environmentRef, err := environmentScopeRefsFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if projectRef == "" && environmentRef == "" && actor.Kind == auth.ActorKindAPIKey {
		scope, ok := actor.EnvironmentScope()
		if !ok {
			writeError(w, badRequest(errors.New("API key is not bound to an environment")))
			return
		}
		projectRef = scope.ProjectID
		environmentRef = scope.EnvironmentID
	}
	request.ProjectID = projectRef
	request.EnvironmentID = environmentRef
	deployment, scope, projectID, environmentID, err := s.resolvePromotionTarget(r.Context(), store, actor.OrgID, deploymentRef, request)
	if isNoRows(err) {
		writeError(w, notFound(errors.New("deployment not found")))
		return
	}
	if errors.Is(err, errAmbiguousDeploymentVersion) {
		writeError(w, badRequest(err))
		return
	}
	if err != nil {
		s.log.Error("get deployment for promotion failed", "deployment", deploymentRef, "error", err)
		writeError(w, errors.New("get deployment"))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	principal, err := auth.ActorPrincipal(actor)
	if err != nil {
		writeError(w, forbidden(err))
		return
	}
	placement, err := s.resolveEnvironmentPlacement(r.Context(), s.db, actor.OrgID, projectID, environmentID)
	if err != nil {
		writeError(w, err)
		return
	}
	params := db.PromoteDeploymentParams{
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:               pgvalue.UUID(actor.OrgID),
		WorkerGroupID:       placement.WorkerGroupID,
		ProjectID:           projectID,
		EnvironmentID:       environmentID,
		DeploymentID:        deployment.ID,
		PromotedByPrincipal: principal,
		Reason:              strings.TrimSpace(request.Reason),
	}
	promoteStore := interface {
		PromoteDeployment(context.Context, db.PromoteDeploymentParams) (db.PromoteDeploymentRow, error)
	}(store)
	err = s.inTx(r.Context(), func(work *txWork) error {
		txPromoteStore, ok := work.q.(interface {
			PromoteDeployment(context.Context, db.PromoteDeploymentParams) (db.PromoteDeploymentRow, error)
		})
		if !ok {
			return unavailable(errors.New("deployment storage is not configured"))
		}
		promoteStore = txPromoteStore
		changedSchedules, err := promoteDeploymentAndSyncSchedules(r.Context(), promoteStore, params)
		if isNoRows(err) {
			return badRequest(errors.New("deployment is not deployable"))
		} else if errors.Is(err, errPermissionRequired) {
			return forbidden(err)
		} else if err != nil {
			s.log.Error("promote deployment failed", "deployment", deploymentRef, "error", err)
			return errors.New("promote deployment")
		}
		work.AfterCommit(func(ctx context.Context) {
			s.registerChangedScheduleInstances(ctx, params.OrgID, params.ProjectID, changedSchedules)
			s.reconcilePreparedRuntimeSupplyAsync(ctx, "deployment_promotion")
		})
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	response, err := deploymentResponseWithArtifacts(r.Context(), store, deployment)
	if err != nil {
		s.log.Error("get promoted deployment artifacts failed", "deployment", deploymentRef, "error", err)
		writeError(w, errors.New("promote deployment"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func validateDeploymentContentHash(archivePath string, contentHash string) error {
	contentHash = strings.TrimSpace(contentHash)
	if contentHash == "" {
		return errors.New("deployment content_hash is required")
	}
	digest, err := deploymentArchiveDigest(archivePath)
	if err != nil {
		return fmt.Errorf("hash deployment source artifact: %w", err)
	}
	if digest != contentHash {
		return fmt.Errorf("deployment source content_hash %s does not match uploaded archive digest %s", contentHash, digest)
	}
	return nil
}

func createDeploymentRecords(ctx context.Context, store deploymentStore, workerGroupID string, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, contentHash string, artifact api.DeploymentSourceArtifact, metadata deploymentVersionMetadata) (api.DeploymentResponse, error) {
	if _, err := store.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		OrgID:     pgvalue.UUID(orgID),
		Digest:    artifact.Digest,
		SizeBytes: artifact.SizeBytes,
		MediaType: artifact.MediaType,
	}); err != nil {
		return api.DeploymentResponse{}, err
	}
	if err := store.LockDeploymentReusableBuildKey(ctx, db.LockDeploymentReusableBuildKeyParams{
		OrgID:              pgvalue.UUID(orgID),
		BuildWorkerGroupID: workerGroupID,
		ProjectID:          projectID,
		EnvironmentID:      environmentID,
		WorkerGroupID:      workerGroupID,
		ContentHash:        contentHash,
	}); err != nil {
		return api.DeploymentResponse{}, err
	}
	deployment, err := store.GetReusableDeploymentByContentHash(ctx, db.GetReusableDeploymentByContentHashParams{
		OrgID:              pgvalue.UUID(orgID),
		BuildWorkerGroupID: workerGroupID,
		ProjectID:          projectID,
		EnvironmentID:      environmentID,
		ContentHash:        contentHash,
		WorkerGroupID:      workerGroupID,
	})
	if isNoRows(err) {
		deployment, err = createQueuedDeployment(ctx, store, workerGroupID, orgID, projectID, environmentID, contentHash, artifact, metadata)
	}
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	response, err := deploymentResponseWithArtifacts(ctx, store, deployment)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	return response, nil
}

func createQueuedDeployment(ctx context.Context, store deploymentStore, workerGroupID string, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, contentHash string, artifact api.DeploymentSourceArtifact, metadata deploymentVersionMetadata) (db.Deployment, error) {
	version, err := nextDeploymentVersion(ctx, store, orgID, projectID, environmentID)
	if err != nil {
		return db.Deployment{}, err
	}
	sourceArtifact, err := store.CreateArtifact(ctx, db.CreateArtifactParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Digest:        artifact.Digest,
		Kind:          db.ArtifactKindDeploymentSource,
		SizeBytes:     artifact.SizeBytes,
		MediaType:     artifact.MediaType,
	})
	if err != nil {
		return db.Deployment{}, err
	}
	var publicID string
	deployment, err := createWithPublicID(ctx, []publicIDSlot{{prefix: publicid.Deployment, value: &publicID}}, func() (db.Deployment, error) {
		return store.CreateDeployment(ctx, db.CreateDeploymentParams{
			ID:                         pgvalue.UUID(uuid.Must(uuid.NewV7())),
			PublicID:                   publicID,
			OrgID:                      pgvalue.UUID(orgID),
			BuildWorkerGroupID:         workerGroupID,
			ProjectID:                  projectID,
			EnvironmentID:              environmentID,
			Version:                    version,
			ApiVersion:                 metadata.APIVersion,
			SdkVersion:                 metadata.SDKVersion,
			CliVersion:                 metadata.CLIVersion,
			BundleFormatVersion:        metadata.BundleFormatVersion,
			WorkerProtocolVersion:      metadata.WorkerProtocolVersion,
			WorkerGroupID:              workerGroupID,
			ContentHash:                contentHash,
			DeploymentSourceArtifactID: sourceArtifact.ID,
			Status:                     db.DeploymentStatusQueued,
		})
	})
	if err != nil {
		return db.Deployment{}, err
	}
	if err := appendDeploymentLifecycleEvent(ctx, store, deployment.OrgID, deployment.ProjectID, deployment.EnvironmentID, deployment.ID, "deployment.queued", "info", "control", "queued", "Deployment queued"); err != nil {
		return db.Deployment{}, err
	}
	return deployment, nil
}

func nextDeploymentVersion(ctx context.Context, store deploymentStore, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID) (string, error) {
	return store.AllocateDeploymentVersion(ctx, db.AllocateDeploymentVersionParams{
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Prefix:        deploymentVersionPrefix(),
	})
}

func deploymentVersionPrefix() string {
	return time.Now().UTC().Format("20060102")
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

var errAmbiguousDeploymentVersion = errors.New("deployment version is ambiguous; provide project_id and environment_id")

func (s *Server) resolvePromotionTarget(ctx context.Context, store deploymentStatusStore, orgID uuid.UUID, deploymentRef string, request api.PromoteDeploymentRequest) (db.Deployment, auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	projectRef := strings.TrimSpace(request.ProjectID)
	environmentRef := strings.TrimSpace(request.EnvironmentID)
	if projectRef != "" || environmentRef != "" {
		scope, projectID, environmentID, err := s.secretRequestScope(ctx, orgID, projectRef, environmentRef)
		if err != nil {
			return db.Deployment{}, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
		}
		if _, placementErr := s.requireEnvironmentPlacementWorkerGroup(ctx, s.db, orgID, projectID, environmentID); placementErr != nil {
			return db.Deployment{}, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, placementErr
		}
		deployment, err := deploymentByIDOrVersion(ctx, store, orgID, projectID, environmentID, deploymentRef)
		return deployment, scope, projectID, environmentID, err
	}
	deployment, err := deploymentByIDOrVersionForOrg(ctx, store, orgID, deploymentRef)
	if err != nil {
		return db.Deployment{}, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return deployment, auth.Scope{
		OrgID:         orgID,
		ProjectID:     pgvalue.MustUUIDValue(deployment.ProjectID).String(),
		EnvironmentID: pgvalue.MustUUIDValue(deployment.EnvironmentID).String(),
	}, deployment.ProjectID, deployment.EnvironmentID, nil
}

func deploymentByIDOrVersionForOrg(ctx context.Context, store deploymentStatusStore, orgID uuid.UUID, deploymentRef string) (db.Deployment, error) {
	if deploymentID, err := uuid.Parse(deploymentRef); err == nil {
		return store.GetDeploymentForOrg(ctx, db.GetDeploymentForOrgParams{
			OrgID: pgvalue.UUID(orgID),
			ID:    pgvalue.UUID(deploymentID),
		})
	}
	deployments, err := store.ListDeploymentsByVersionForOrg(ctx, db.ListDeploymentsByVersionForOrgParams{
		OrgID:   pgvalue.UUID(orgID),
		Version: strings.TrimSpace(deploymentRef),
	})
	if err != nil {
		return db.Deployment{}, err
	}
	switch len(deployments) {
	case 0:
		return db.Deployment{}, errRecordNotFound
	case 1:
		return deployments[0], nil
	default:
		return db.Deployment{}, errAmbiguousDeploymentVersion
	}
}

func deploymentByIDOrVersion(ctx context.Context, store deploymentStatusStore, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, deploymentRef string) (db.Deployment, error) {
	if deploymentID, err := uuid.Parse(deploymentRef); err == nil {
		return store.GetDeployment(ctx, db.GetDeploymentParams{
			OrgID:         pgvalue.UUID(orgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ID:            pgvalue.UUID(deploymentID),
		})
	}
	return store.GetDeploymentByVersion(ctx, db.GetDeploymentByVersionParams{
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Version:       strings.TrimSpace(deploymentRef),
	})
}

func deploymentResponse(deployment db.Deployment, artifact api.DeploymentSourceArtifact, buildManifestDigest string, deploymentManifestDigest string) api.DeploymentResponse {
	return api.DeploymentResponse{
		ID:                       pgvalue.MustUUIDValue(deployment.ID).String(),
		Version:                  deployment.Version,
		APIVersion:               deployment.ApiVersion,
		SDKVersion:               deployment.SdkVersion,
		CLIVersion:               deployment.CliVersion,
		BundleFormatVersion:      deployment.BundleFormatVersion,
		WorkerProtocolVersion:    deployment.WorkerProtocolVersion,
		ProjectID:                pgvalue.MustUUIDValue(deployment.ProjectID).String(),
		EnvironmentID:            pgvalue.MustUUIDValue(deployment.EnvironmentID).String(),
		ContentHash:              deployment.ContentHash,
		DeploymentSource:         artifact,
		BuildManifestDigest:      buildManifestDigest,
		DeploymentManifestDigest: deploymentManifestDigest,
		Status:                   string(deployment.Status),
		Error:                    deploymentErrorResponse(deployment.Failure),
		CreatedAt:                pgvalue.Time(deployment.CreatedAt),
		BuildingAt:               pgvalue.Time(deployment.BuildingAt),
		BuiltAt:                  pgvalue.Time(deployment.BuiltAt),
		DeployedAt:               pgvalue.Time(deployment.DeployedAt),
		FailedAt:                 pgvalue.Time(deployment.FailedAt),
	}
}

func deploymentErrorResponse(raw []byte) *api.DeploymentErrorResponse {
	message := strings.TrimSpace(string(raw))
	if message == "" || message == "null" {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return &api.DeploymentErrorResponse{Message: message}
	}
	if value, ok := payload["message"].(string); ok && strings.TrimSpace(value) != "" {
		return &api.DeploymentErrorResponse{Message: strings.TrimSpace(value)}
	}
	if value, ok := payload["error"].(string); ok && strings.TrimSpace(value) != "" {
		return &api.DeploymentErrorResponse{Message: strings.TrimSpace(value)}
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		if value, ok := nested["message"].(string); ok && strings.TrimSpace(value) != "" {
			return &api.DeploymentErrorResponse{Message: strings.TrimSpace(value)}
		}
	}
	return nil
}

func int8Response(value pgtype.Int8) *int32 {
	if !value.Valid {
		return nil
	}
	out := int32(value.Int64)
	return &out
}

func writeDeploymentError(w http.ResponseWriter, s *Server, err error) {
	if isUniqueViolation(err) {
		writeError(w, badRequest(errors.New("deployment conflicts with existing task metadata")))
		return
	}
	s.log.Error("create deployment failed", "error", err)
	writeError(w, errors.New("create deployment"))
}
