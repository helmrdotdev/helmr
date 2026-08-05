package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type deploymentStore interface {
	AppendDeploymentEvent(context.Context, db.AppendDeploymentEventParams) (db.AppendDeploymentEventRow, error)
	CreateArtifact(context.Context, db.CreateArtifactParams) (db.Artifact, error)
	CreateDeployment(context.Context, db.CreateDeploymentParams) (db.Deployment, error)
	ListArtifactsByIDs(context.Context, db.ListArtifactsByIDsParams) ([]db.Artifact, error)
	UpsertCasObject(context.Context, db.UpsertCasObjectParams) (db.CasObject, error)
}

type deploymentCreationReceipt struct {
	DeploymentID string `json:"deploymentId"`
}

type currentDeploymentStore interface {
	GetCurrentDeployment(context.Context, db.GetCurrentDeploymentParams) (db.Deployment, error)
	ListArtifactsByIDs(context.Context, db.ListArtifactsByIDsParams) ([]db.Artifact, error)
}

type deploymentStatusStore interface {
	GetDeployment(context.Context, db.GetDeploymentParams) (db.Deployment, error)
	GetDeploymentForOrg(context.Context, db.GetDeploymentForOrgParams) (db.Deployment, error)
	ListArtifactsByIDs(context.Context, db.ListArtifactsByIDsParams) ([]db.Artifact, error)
	ListScopedDeployments(context.Context, db.ListScopedDeploymentsParams) ([]db.Deployment, error)
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
		response = append(response, item)
	}
	writeJSON(w, http.StatusOK, api.ListDeploymentsResponse{Deployments: response})
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
	response, err := deploymentResponseWithArtifacts(r.Context(), store, deployment)
	if err != nil {
		s.log.Error("get deployment artifacts failed", "deployment_id", deploymentID.String(), "error", err)
		writeError(w, errors.New("get deployment"))
		return
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
	response, err := deploymentResponseWithArtifacts(r.Context(), store, deployment)
	if err != nil {
		s.log.Error("get current deployment artifacts failed", "deployment_id", pgvalue.MustUUIDValue(deployment.ID).String(), "error", err)
		writeError(w, errors.New("get current deployment"))
		return
	}
	writeJSON(w, http.StatusOK, api.GetCurrentDeploymentResponse{Deployment: &response})
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

func createDeploymentRecords(
	ctx context.Context,
	store deploymentStore,
	buildRegionID string,
	selection deployment.SourceSelection,
	orgID uuid.UUID,
	projectID pgtype.UUID,
	environmentID pgtype.UUID,
	contentHash string,
	artifact api.DeploymentSourceArtifact,
	metadata deploymentVersionMetadata,
) (api.DeploymentResponse, error) {
	if _, err := store.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		OrgID:     pgvalue.UUID(orgID),
		Digest:    artifact.Digest,
		SizeBytes: artifact.SizeBytes,
		MediaType: artifact.MediaType,
	}); err != nil {
		return api.DeploymentResponse{}, err
	}
	deployment, err := createQueuedDeployment(
		ctx,
		store,
		buildRegionID,
		selection,
		orgID,
		projectID,
		environmentID,
		contentHash,
		artifact,
		metadata,
	)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	response, err := deploymentResponseWithArtifacts(ctx, store, deployment)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	return response, nil
}

func completeDeploymentCreation(
	ctx context.Context,
	claims *idempotency.Transaction,
	claim db.IdempotencyClaim,
	deploymentID string,
) error {
	receipt, err := json.Marshal(deploymentCreationReceipt{DeploymentID: deploymentID})
	if err != nil {
		return fmt.Errorf("encode deployment creation receipt: %w", err)
	}
	if _, err := claims.Complete(ctx, claim, receipt); err != nil {
		return fmt.Errorf("complete deployment creation claim: %w", err)
	}
	return nil
}

func replayDeploymentCreation(
	ctx context.Context,
	queries db.Querier,
	store deploymentStore,
	claim db.IdempotencyClaim,
	orgID pgtype.UUID,
	projectID pgtype.UUID,
) (api.DeploymentResponse, error) {
	if claim.State != "completed" {
		return api.DeploymentResponse{}, conflict(errors.New("deployment creation is in progress"))
	}
	var receipt deploymentCreationReceipt
	decoder := json.NewDecoder(strings.NewReader(string(claim.Receipt)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return api.DeploymentResponse{}, errors.New("deployment creation receipt is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return api.DeploymentResponse{}, errors.New("deployment creation receipt is invalid")
	}
	deploymentID, err := ids.Parse(receipt.DeploymentID)
	if err != nil {
		return api.DeploymentResponse{}, errors.New("deployment creation receipt is invalid")
	}
	record, err := queries.GetDeployment(ctx, db.GetDeploymentParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: claim.EnvironmentID,
		ID:            pgvalue.UUID(deploymentID),
	})
	if err != nil {
		return api.DeploymentResponse{}, fmt.Errorf("resolve replayed deployment: %w", err)
	}
	return deploymentResponseWithArtifacts(ctx, store, record)
}

func createQueuedDeployment(
	ctx context.Context,
	store deploymentStore,
	buildRegionID string,
	selection deployment.SourceSelection,
	orgID uuid.UUID,
	projectID pgtype.UUID,
	environmentID pgtype.UUID,
	contentHash string,
	artifact api.DeploymentSourceArtifact,
	metadata deploymentVersionMetadata,
) (db.Deployment, error) {
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
	deploymentID := uuid.Must(uuid.NewV7())
	deployment, err := store.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID:                  pgvalue.UUID(deploymentID),
		OrgID:               pgvalue.UUID(orgID),
		BuildRegionID:       buildRegionID,
		BuildNodeVersion:    selection.NodeVersion,
		BuildManagerName:    string(selection.Manager.Name),
		BuildManagerVersion: selection.Manager.Version,
		BuildManagerIntegrity: pgtype.Text{
			String: selection.Manager.Integrity,
			Valid:  selection.Manager.Integrity != "",
		},
		BuildContractVersion:       deployment.ProgramBuildContractVersion,
		ImageCacheMode:             metadata.ImageCacheMode,
		ProjectID:                  projectID,
		EnvironmentID:              environmentID,
		Version:                    deploymentVersion(deploymentID),
		APIVersion:                 metadata.APIVersion,
		WorkerProtocolVersion:      metadata.WorkerProtocolVersion,
		ContentHash:                contentHash,
		DeploymentSourceArtifactID: sourceArtifact.ID,
		Status:                     db.DeploymentStatusQueued,
	})
	if err != nil {
		return db.Deployment{}, err
	}
	if err := appendDeploymentLifecycleEvent(ctx, store, deployment.OrgID, deployment.ProjectID, deployment.EnvironmentID, deployment.ID, "deployment.queued", "info", "control", "queued", "Deployment queued"); err != nil {
		return db.Deployment{}, err
	}
	return deployment, nil
}

func deploymentVersion(id uuid.UUID) string {
	seconds, nanoseconds := id.Time().UnixTime()
	return time.Unix(seconds, nanoseconds).UTC().Format("20060102") + "." + id.String()
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func firstPresentString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func deploymentResponse(deployment db.Deployment, artifact api.DeploymentSourceArtifact) api.DeploymentResponse {
	return api.DeploymentResponse{
		ID:                    pgvalue.MustUUIDValue(deployment.ID).String(),
		Version:               deployment.Version,
		APIVersion:            deployment.APIVersion,
		WorkerProtocolVersion: deployment.WorkerProtocolVersion,
		ProjectID:             pgvalue.MustUUIDValue(deployment.ProjectID).String(),
		EnvironmentID:         pgvalue.MustUUIDValue(deployment.EnvironmentID).String(),
		ContentHash:           deployment.ContentHash,
		DeploymentSource:      artifact,
		Status:                string(deployment.Status),
		Error:                 deploymentErrorResponse(deployment.Failure),
		CreatedAt:             pgvalue.Time(deployment.CreatedAt),
		BuildingAt:            pgvalue.Time(deployment.BuildingAt),
		BuiltAt:               pgvalue.Time(deployment.BuiltAt),
		DeployedAt:            pgvalue.Time(deployment.DeployedAt),
		FailedAt:              pgvalue.Time(deployment.FailedAt),
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

func writeDeploymentError(w http.ResponseWriter, s *Server, err error) {
	var idempotencyConflict idempotency.ConflictError
	if errors.As(err, &idempotencyConflict) {
		writeError(w, conflict(errors.New("idempotency key conflicts with an existing deployment request")))
		return
	}
	if isUniqueViolation(err) {
		writeError(w, badRequest(errors.New("deployment conflicts with existing task metadata")))
		return
	}
	if errorStatus(err) != http.StatusInternalServerError {
		writeError(w, err)
		return
	}
	s.log.Error("create deployment failed", "error", err)
	writeError(w, errors.New("create deployment"))
}
