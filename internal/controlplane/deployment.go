package controlplane

import (
	"context"
	"encoding/base64"
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
	ListScopedDeployments(context.Context, db.ListScopedDeploymentsParams) ([]db.ListScopedDeploymentsRow, error)
}

const (
	deploymentListDefaultLimit = int32(50)
	deploymentListMaxLimit     = int32(100)
)

type deploymentListCursor struct {
	ProjectID     string    `json:"project_id"`
	EnvironmentID string    `json:"environment_id"`
	CreatedAt     time.Time `json:"created_at"`
	ID            string    `json:"id"`
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
	limit, cursor, err := parseDeploymentListQuery(r, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	params := db.ListScopedDeploymentsParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		RowLimit:      limit + 1,
	}
	if cursor != nil {
		params.HasAfter = true
		params.AfterCreatedAt = pgvalue.Timestamptz(cursor.CreatedAt)
		params.AfterID = pgvalue.UUID(uuid.MustParse(cursor.ID))
	}
	rows, err := store.ListScopedDeployments(r.Context(), params)
	if err != nil {
		s.log.Error("list deployments failed", "error", err)
		writeError(w, errors.New("list deployments"))
		return
	}
	hasMore := len(rows) > int(limit)
	if hasMore {
		rows = rows[:limit]
	}
	response := make([]api.DeploymentListItem, 0, len(rows))
	for _, row := range rows {
		status, err := deploymentPublicStatus(row.Status)
		if err != nil {
			s.log.Error("project deployment list item failed", "deployment_id", pgvalue.UUIDString(row.ID), "error", err)
			writeError(w, errors.New("list deployments"))
			return
		}
		response = append(response, api.DeploymentListItem{
			ID: pgvalue.UUIDString(row.ID), Version: row.Version, Status: status,
			CreatedAt: pgvalue.Time(row.CreatedAt), BuildingAt: pgvalue.TimePtr(row.BuildingAt),
			BuiltAt: pgvalue.TimePtr(row.BuiltAt), DeployedAt: pgvalue.TimePtr(row.DeployedAt),
			FailedAt: pgvalue.TimePtr(row.FailedAt),
		})
	}
	page := api.ListDeploymentsResponse{Deployments: response}
	if hasMore {
		last := rows[len(rows)-1]
		page.NextCursor, err = encodeDeploymentListCursor(deploymentListCursor{
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			CreatedAt: pgvalue.Time(last.CreatedAt), ID: pgvalue.UUIDString(last.ID),
		})
		if err != nil {
			writeError(w, errors.New("list deployments"))
			return
		}
	}
	writeJSON(w, http.StatusOK, page)
}

func parseDeploymentListQuery(r *http.Request, projectID, environmentID string) (int32, *deploymentListCursor, error) {
	values := r.URL.Query()
	for name, entries := range values {
		if name != "cursor" && name != "limit" {
			return 0, nil, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || strings.TrimSpace(entries[0]) == "" {
			return 0, nil, fmt.Errorf("query parameter %q must appear once", name)
		}
	}
	limit := deploymentListDefaultLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > int64(deploymentListMaxLimit) {
			return 0, nil, errors.New("limit must be an integer in [1,100]")
		}
		limit = int32(parsed)
	}
	if raw := values.Get("cursor"); raw != "" {
		cursor, err := decodeDeploymentListCursor(raw)
		if err != nil {
			return 0, nil, err
		}
		if cursor.ProjectID != projectID || cursor.EnvironmentID != environmentID {
			return 0, nil, errors.New("deployment cursor does not match request scope")
		}
		return limit, &cursor, nil
	}
	return limit, nil, nil
}

func encodeDeploymentListCursor(cursor deploymentListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeDeploymentListCursor(raw string) (deploymentListCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return deploymentListCursor{}, errors.New("deployment cursor is invalid")
	}
	var cursor deploymentListCursor
	if json.Unmarshal(decoded, &cursor) != nil || cursor.ProjectID == "" ||
		cursor.EnvironmentID == "" || cursor.CreatedAt.IsZero() || ids.Validate(cursor.ID) != nil {
		return deploymentListCursor{}, errors.New("deployment cursor is invalid")
	}
	return cursor, nil
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
		writeError(w, notFound(codedError{
			code: "no_current_deployment", message: "no current deployment",
		}))
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
		BuildContract:              deployment.ProgramBuildContract,
		ImageCacheMode:             metadata.ImageCacheMode,
		ProjectID:                  projectID,
		EnvironmentID:              environmentID,
		Version:                    deploymentVersion(deploymentID),
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

func deploymentResponse(deployment db.Deployment, artifact api.DeploymentSourceArtifact) (api.DeploymentResponse, error) {
	status, err := deploymentPublicStatus(deployment.Status)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	failure, err := deploymentFailureResponse(deployment.Failure)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	if (status == api.DeploymentStatusFailed) != (failure != nil) {
		return api.DeploymentResponse{}, errors.New("deployment failure projection is inconsistent")
	}
	return api.DeploymentResponse{
		ID:               pgvalue.MustUUIDValue(deployment.ID).String(),
		Version:          deployment.Version,
		ContentHash:      deployment.ContentHash,
		DeploymentSource: artifact,
		Status:           status,
		Failure:          failure,
		CreatedAt:        pgvalue.Time(deployment.CreatedAt),
		BuildingAt:       pgvalue.TimePtr(deployment.BuildingAt),
		BuiltAt:          pgvalue.TimePtr(deployment.BuiltAt),
		DeployedAt:       pgvalue.TimePtr(deployment.DeployedAt),
		FailedAt:         pgvalue.TimePtr(deployment.FailedAt),
	}, nil
}

func deploymentPublicStatus(status db.DeploymentStatus) (api.DeploymentStatus, error) {
	switch status {
	case db.DeploymentStatusQueued:
		return api.DeploymentStatusQueued, nil
	case db.DeploymentStatusBuilding:
		return api.DeploymentStatusBuilding, nil
	case db.DeploymentStatusDeployed:
		return api.DeploymentStatusDeployed, nil
	case db.DeploymentStatusFailed:
		return api.DeploymentStatusFailed, nil
	default:
		return "", fmt.Errorf("deployment status %q has no public projection", status)
	}
}

func deploymentFailureResponse(raw []byte) (*api.DeploymentFailure, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var failure api.DeploymentFailure
	if err := json.Unmarshal(raw, &failure); err != nil || failure.Code == "" ||
		failure.Message == "" || len(failure.Details) == 0 {
		return nil, errors.New("deployment failure is invalid")
	}
	var details map[string]json.RawMessage
	if err := json.Unmarshal(failure.Details, &details); err != nil || details == nil {
		return nil, errors.New("deployment failure details are invalid")
	}
	return &failure, nil
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
