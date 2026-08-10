package controlplane

import (
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/capacityapi"
	"github.com/helmrdotdev/helmr/internal/auth"
	capacityplanner "github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	capacityRequestBodyLimit      = int64(16 << 10)
	defaultCapacityInstanceLimit  = int32(200)
	maximumCapacityInstanceLimit  = int32(500)
	maximumCapacityResourceIDSize = 512
	capacityTokenDecodedByteCount = 32
)

var capacityInstanceStatuses = map[string]struct{}{
	string(db.WorkerInstanceStateRegistering):      {},
	string(db.WorkerInstanceStateActive):           {},
	string(db.WorkerInstanceStateDraining):         {},
	string(db.WorkerInstanceStateTerminationReady): {},
	string(db.WorkerInstanceStateLost):             {},
}

func hashCapacityToken(raw string) ([]byte, error) {
	if raw == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) != capacityTokenDecodedByteCount || base64.RawURLEncoding.EncodeToString(decoded) != raw {
		return nil, errors.New("capacity token must be a canonical base64url-no-pad encoding of exactly 32 bytes")
	}
	return auth.HashCredential(raw), nil
}

func (s *Server) mountCapacityRoutes(r chi.Router) {
	r.Route(capacityapi.RoutePrefix, func(r chi.Router) {
		r.Use(s.requireCapacity)
		r.Get(capacityapi.WorkerGroupsPath+"/resolve", s.capacityResolveWorkerGroup)
		r.Get(capacityapi.WorkerGroupsPath+"/{workerGroupID}/pools/resolve", s.capacityResolveWorkerPool)
		r.With(limitRequestBody(capacityRequestBodyLimit)).
			Put(capacityapi.WorkerGroupsPath+"/{workerGroupID}/primary-pools", s.capacityReconcileWorkerGroupPrimaryPools)
		r.With(limitRequestBody(capacityRequestBodyLimit)).
			Post(capacityapi.WorkerGroupsPath+"/{workerGroupID}/plan", s.capacityPlan)
		r.Get(capacityapi.WorkerInstancesPath, s.capacityListWorkerInstances)
		r.Get(capacityapi.WorkerInstancesPath+"/{workerInstanceID}", s.capacityGetWorkerInstance)
		r.With(limitRequestBody(capacityRequestBodyLimit)).
			Post(capacityapi.WorkerInstancesPath+"/{workerInstanceID}/drain", s.capacityDrainWorkerInstance)
	})
}

func (s *Server) capacityResolveWorkerPool(w http.ResponseWriter, r *http.Request) {
	workerGroupID, err := ids.Parse(chi.URLParam(r, "workerGroupID"))
	if err != nil {
		writeError(w, badRequest(errors.New("worker_group_id must be a canonical UUIDv7")))
		return
	}
	query := r.URL.Query()
	if len(query) != 1 || len(query["name"]) != 1 {
		writeError(w, badRequest(errors.New("name is required exactly once")))
		return
	}
	name := query.Get("name")
	if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name {
		writeError(w, badRequest(errors.New("name must be non-empty and canonical")))
		return
	}
	pool, err := s.db.GetWorkerPoolByGroupName(r.Context(), db.GetWorkerPoolByGroupNameParams{
		WorkerGroupID: workerGroupID.String(), Name: name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("worker pool was not found")))
		return
	}
	if err != nil {
		s.log.Error("resolve capacity Worker pool", "worker_group_id", workerGroupID.String(), "name", name, "error", err)
		writeError(w, errors.New("resolve capacity worker pool"))
		return
	}
	status, err := workerPoolPublicStatus(pool.State)
	if err != nil {
		writeError(w, errors.New("project capacity worker pool"))
		return
	}
	writeJSON(w, http.StatusOK, capacityapi.CapacityWorkerPool{
		ID: pgvalue.MustUUIDValue(pool.ID).String(), WorkerGroupID: pool.WorkerGroupID,
		Name: pool.Name, Status: status, AllowsRun: pool.AllowsRun, AllowsBuild: pool.AllowsBuild,
	})
}

func (s *Server) capacityResolveWorkerGroup(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if len(query) != 2 || len(query["region_id"]) != 1 || len(query["name"]) != 1 {
		writeError(w, badRequest(errors.New("region_id and name are required exactly once")))
		return
	}
	regionID := query.Get("region_id")
	name := query.Get("name")
	if strings.TrimSpace(regionID) == "" || strings.TrimSpace(regionID) != regionID || strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name {
		writeError(w, badRequest(errors.New("region_id and name must be non-empty and canonical")))
		return
	}
	group, err := s.db.GetWorkerGroupByRegionName(r.Context(), db.GetWorkerGroupByRegionNameParams{RegionID: regionID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("worker group was not found")))
		return
	}
	if err != nil {
		s.log.Error("resolve capacity Worker group", "region_id", regionID, "name", name, "error", err)
		writeError(w, errors.New("resolve capacity worker group"))
		return
	}
	status, err := workerGroupPublicStatus(group.State)
	if err != nil {
		writeError(w, errors.New("project capacity worker group"))
		return
	}
	writeJSON(w, http.StatusOK, capacityWorkerGroup(group, status))
}

func (s *Server) capacityReconcileWorkerGroupPrimaryPools(w http.ResponseWriter, r *http.Request) {
	workerGroupID, err := ids.Parse(chi.URLParam(r, "workerGroupID"))
	if err != nil {
		writeError(w, badRequest(errors.New("worker_group_id must be a canonical UUIDv7")))
		return
	}
	var request capacityapi.ReconcileWorkerGroupPrimaryPoolsRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid primary Pool selection JSON: %w", err)))
		return
	}
	if request.ExpectedGroupClaimVersion <= 0 {
		writeError(w, badRequest(errors.New("expected_group_claim_version must be positive")))
		return
	}
	runPoolID, err := capacityOptionalPoolID(request.RunPoolID)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("run_pool_id: %w", err)))
		return
	}
	buildPoolID, err := capacityOptionalPoolID(request.BuildPoolID)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("build_pool_id: %w", err)))
		return
	}
	result, err := s.reconcileWorkerGroupPrimarySelection(r.Context(), workerGroupPrimarySelectionCommand{
		workerGroupID:             workerGroupID.String(),
		expectedGroupClaimVersion: request.ExpectedGroupClaimVersion,
		requireCompleteSelection:  true,
		desired: func(db.WorkerGroup) (workerGroupPrimarySelection, error) {
			return workerGroupPrimarySelection{runPoolID: runPoolID, buildPoolID: buildPoolID}, nil
		},
	})
	if err != nil {
		writeError(w, err)
		return
	}
	status, err := workerGroupPublicStatus(result.group.State)
	if err != nil {
		writeError(w, errors.New("project capacity worker group"))
		return
	}
	writeJSON(w, http.StatusOK, capacityapi.ReconcileWorkerGroupPrimaryPoolsResponse{
		WorkerGroup: capacityWorkerGroup(result.group, status),
		Applied:     result.applied,
	})
}

func capacityOptionalPoolID(raw string) (pgtype.UUID, error) {
	if raw == "" {
		return pgtype.UUID{}, nil
	}
	id, err := ids.Parse(raw)
	if err != nil || id.String() != raw {
		return pgtype.UUID{}, errors.New("must be empty or a canonical UUIDv7")
	}
	return pgvalue.UUID(id), nil
}

func capacityWorkerGroup(group db.WorkerGroup, status capacityapi.WorkerGroupStatus) capacityapi.CapacityWorkerGroup {
	return capacityapi.CapacityWorkerGroup{
		ID: group.ID, Name: group.Name, RegionID: group.RegionID, Status: status,
		ClaimVersion: group.ClaimVersion, AllowsRun: group.AllowsRun, AllowsBuild: group.AllowsBuild,
		PrimaryRunPoolID:   pgvalue.UUIDString(group.PrimaryRunPoolID),
		PrimaryBuildPoolID: pgvalue.UUIDString(group.PrimaryBuildPoolID),
	}
}

func (s *Server) requireCapacity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || len(s.capacityTokenHash) == 0 || !hmac.Equal(auth.HashCredential(raw), s.capacityTokenHash) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, unauthorized(errors.New("deployment capacity authentication is required")))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) capacityPlan(w http.ResponseWriter, r *http.Request) {
	workerGroupID, err := ids.Parse(chi.URLParam(r, "workerGroupID"))
	if err != nil {
		writeError(w, badRequest(errors.New("worker_group_id must be a canonical UUIDv7")))
		return
	}
	var request capacityapi.CapacityPlanRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid capacity plan JSON: %w", err)))
		return
	}
	response, err := capacityplanner.Plan(r.Context(), s.db, workerGroupID.String(), request, time.Now())
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("worker group was not found")))
		return
	}
	if err != nil {
		if errors.Is(err, capacityplanner.ErrInvalidPlanRequest) {
			writeError(w, badRequest(err))
			return
		}
		s.log.Error("plan deployment capacity", "worker_group_id", workerGroupID.String(), "error", err)
		writeError(w, errors.New("plan deployment capacity"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) capacityListWorkerInstances(w http.ResponseWriter, r *http.Request) {
	params, err := capacityWorkerInstanceListParams(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	rows, err := s.db.ListCapacityWorkerInstances(r.Context(), params)
	if err != nil {
		s.log.Error("list capacity Worker instances", "error", err)
		writeError(w, errors.New("list capacity worker instances"))
		return
	}
	response := capacityapi.WorkerInstancesResponse{
		WorkerInstances: make([]capacityapi.WorkerInstance, 0, len(rows)),
	}
	for _, row := range rows {
		projected, err := capacityWorkerInstance(
			row.ID, row.ResourceID, row.WorkerGroupID, row.WorkerPoolID, row.State, row.ClaimVersion,
			row.CurrentEpoch, row.SupportsRun, row.SupportsBuild, row.DrainingAt,
			row.TerminationReadyAt, row.LostAt, row.CreatedAt, row.UpdatedAt,
		)
		if err != nil {
			s.log.Error("project capacity Worker instance", "error", err)
			writeError(w, errors.New("project capacity worker instance"))
			return
		}
		response.WorkerInstances = append(response.WorkerInstances, projected)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) capacityGetWorkerInstance(w http.ResponseWriter, r *http.Request) {
	id, err := capacityWorkerInstanceID(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	row, err := s.db.GetCapacityWorkerInstance(r.Context(), pgvalue.UUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("worker instance was not found")))
		return
	}
	if err != nil {
		s.log.Error("get capacity Worker instance", "worker_instance_id", id.String(), "error", err)
		writeError(w, errors.New("get capacity worker instance"))
		return
	}
	response, err := capacityWorkerInstance(
		row.ID, row.ResourceID, row.WorkerGroupID, row.WorkerPoolID, row.State, row.ClaimVersion,
		row.CurrentEpoch, row.SupportsRun, row.SupportsBuild, row.DrainingAt,
		row.TerminationReadyAt, row.LostAt, row.CreatedAt, row.UpdatedAt,
	)
	if err != nil {
		s.log.Error("project capacity Worker instance", "worker_instance_id", id.String(), "error", err)
		writeError(w, errors.New("project capacity worker instance"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) capacityDrainWorkerInstance(w http.ResponseWriter, r *http.Request) {
	id, err := capacityWorkerInstanceID(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var request capacityapi.DrainWorkerInstanceRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker drain JSON: %w", err)))
		return
	}
	if request.ExpectedEpoch <= 0 || request.ExpectedClaimVersion <= 0 {
		writeError(w, badRequest(errors.New("expected_epoch and expected_claim_version must be positive")))
		return
	}
	instance, err := s.db.GetCapacityWorkerInstance(r.Context(), pgvalue.UUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("worker instance was not found")))
		return
	}
	if err != nil {
		s.log.Error("get Worker instance for capacity drain", "worker_instance_id", id.String(), "error", err)
		writeError(w, errors.New("get worker instance for drain"))
		return
	}
	draining, err := s.db.DrainWorkerInstance(r.Context(), db.DrainWorkerInstanceParams{
		ID:                   pgvalue.UUID(id),
		WorkerGroupID:        instance.WorkerGroupID,
		ExpectedEpoch:        pgtype.Int8{Int64: request.ExpectedEpoch, Valid: true},
		ExpectedClaimVersion: request.ExpectedClaimVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("worker drain fence is stale or the instance is not active")))
		return
	}
	if err != nil {
		s.log.Error("capacity drain Worker instance", "worker_instance_id", id.String(), "error", err)
		writeError(w, errors.New("drain worker instance"))
		return
	}
	response, err := capacityWorkerInstance(
		draining.ID, draining.ResourceID, draining.WorkerGroupID, draining.WorkerPoolID,
		string(draining.State), draining.ClaimVersion,
		draining.CurrentEpoch, draining.SupportsRun, draining.SupportsBuild, draining.DrainingAt,
		draining.TerminationReadyAt, draining.LostAt, draining.CreatedAt, draining.UpdatedAt,
	)
	if err != nil {
		s.log.Error("project drained capacity Worker instance", "worker_instance_id", id.String(), "error", err)
		writeError(w, errors.New("project drained capacity worker instance"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func capacityWorkerInstanceID(r *http.Request) (uuid.UUID, error) {
	id, err := ids.Parse(chi.URLParam(r, "workerInstanceID"))
	if err != nil {
		return uuid.Nil, errors.New("worker_instance_id must be a canonical UUIDv7")
	}
	return id, nil
}

func capacityWorkerInstanceListParams(r *http.Request) (db.ListCapacityWorkerInstancesParams, error) {
	params := db.ListCapacityWorkerInstancesParams{
		ResourceIds: []string{},
		States:      []string{},
		RowLimit:    defaultCapacityInstanceLimit,
	}
	query := r.URL.Query()
	for name := range query {
		switch name {
		case "worker_group_id", "resource_id", "status", "limit":
		default:
			return params, fmt.Errorf("query parameter %q is not supported", name)
		}
	}
	if len(query["worker_group_id"]) > 1 || len(query["limit"]) > 1 {
		return params, errors.New("worker_group_id and limit must not be repeated")
	}
	if groupID := strings.TrimSpace(query.Get("worker_group_id")); groupID != "" {
		params.WorkerGroupID = pgtype.Text{String: groupID, Valid: true}
	}
	for _, status := range query["status"] {
		status = strings.TrimSpace(status)
		if _, ok := capacityInstanceStatuses[status]; !ok {
			return params, fmt.Errorf("unsupported worker instance status %q", status)
		}
		params.States = append(params.States, status)
	}
	resourceIDs := map[string]struct{}{}
	for _, resourceID := range query["resource_id"] {
		resourceID = strings.TrimSpace(resourceID)
		if resourceID == "" || len(resourceID) > maximumCapacityResourceIDSize {
			return params, fmt.Errorf("resource_id must contain between 1 and %d bytes", maximumCapacityResourceIDSize)
		}
		if _, duplicate := resourceIDs[resourceID]; duplicate {
			return params, fmt.Errorf("resource_id %q is duplicated", resourceID)
		}
		resourceIDs[resourceID] = struct{}{}
		params.ResourceIds = append(params.ResourceIds, resourceID)
	}
	if len(params.ResourceIds) > int(maximumCapacityInstanceLimit) {
		return params, fmt.Errorf("at most %d resource_id filters are allowed", maximumCapacityInstanceLimit)
	}
	if rawLimit := strings.TrimSpace(query.Get("limit")); rawLimit != "" {
		limit, err := strconv.ParseInt(rawLimit, 10, 32)
		if err != nil || limit <= 0 || limit > int64(maximumCapacityInstanceLimit) {
			return params, fmt.Errorf("limit must be between 1 and %d", maximumCapacityInstanceLimit)
		}
		params.RowLimit = int32(limit)
	}
	return params, nil
}

func workerGroupPublicStatus(state string) (capacityapi.WorkerGroupStatus, error) {
	switch state {
	case db.WorkerGroupStateActive:
		return capacityapi.WorkerGroupStatusActive, nil
	case db.WorkerGroupStatePaused:
		return capacityapi.WorkerGroupStatusPaused, nil
	case db.WorkerGroupStateDraining:
		return capacityapi.WorkerGroupStatusDraining, nil
	case db.WorkerGroupStateDisabled:
		return capacityapi.WorkerGroupStatusDisabled, nil
	default:
		return "", fmt.Errorf("worker group state %q has no public projection", state)
	}
}

func workerPoolPublicStatus(state string) (capacityapi.WorkerPoolStatus, error) {
	switch state {
	case "pending":
		return capacityapi.WorkerPoolStatusPending, nil
	case "active":
		return capacityapi.WorkerPoolStatusActive, nil
	case "draining":
		return capacityapi.WorkerPoolStatusDraining, nil
	case "disabled":
		return capacityapi.WorkerPoolStatusDisabled, nil
	default:
		return "", fmt.Errorf("worker pool state %q has no public projection", state)
	}
}

func capacityWorkerInstance(
	id pgtype.UUID,
	resourceID string,
	workerGroupID string,
	workerPoolID pgtype.UUID,
	status string,
	claimVersion int64,
	currentEpoch pgtype.Int8,
	supportsRun bool,
	supportsBuild bool,
	drainingAt pgtype.Timestamptz,
	terminationReadyAt pgtype.Timestamptz,
	lostAt pgtype.Timestamptz,
	createdAt pgtype.Timestamptz,
	updatedAt pgtype.Timestamptz,
) (capacityapi.WorkerInstance, error) {
	publicStatus, err := workerInstancePublicStatus(status)
	if err != nil {
		return capacityapi.WorkerInstance{}, err
	}
	result := capacityapi.WorkerInstance{
		ID: uuid.UUID(id.Bytes).String(), ResourceID: resourceID,
		WorkerGroupID: workerGroupID, WorkerPoolID: uuid.UUID(workerPoolID.Bytes).String(),
		Status: publicStatus, ClaimVersion: claimVersion,
		SupportsRun: supportsRun, SupportsBuild: supportsBuild,
		CreatedAt: createdAt.Time, UpdatedAt: updatedAt.Time,
	}
	if currentEpoch.Valid {
		result.CurrentEpoch = &currentEpoch.Int64
	}
	if drainingAt.Valid {
		result.DrainingAt = &drainingAt.Time
	}
	if terminationReadyAt.Valid {
		result.TerminationReadyAt = &terminationReadyAt.Time
	}
	if lostAt.Valid {
		result.LostAt = &lostAt.Time
	}
	return result, nil
}

func workerInstancePublicStatus(state string) (capacityapi.WorkerInstanceStatus, error) {
	switch state {
	case db.WorkerInstanceStateRegistering:
		return capacityapi.WorkerInstanceStatusRegistering, nil
	case db.WorkerInstanceStateActive:
		return capacityapi.WorkerInstanceStatusActive, nil
	case db.WorkerInstanceStateDraining:
		return capacityapi.WorkerInstanceStatusDraining, nil
	case db.WorkerInstanceStateTerminationReady:
		return capacityapi.WorkerInstanceStatusTerminationReady, nil
	case db.WorkerInstanceStateLost:
		return capacityapi.WorkerInstanceStatusLost, nil
	default:
		return "", fmt.Errorf("worker instance state %q has no public projection", state)
	}
}
