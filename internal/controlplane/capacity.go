package controlplane

import (
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/capacityapi"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
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
		r.Get(capacityapi.ObservationsPath, s.capacityObservations)
		r.Get(capacityapi.WorkerInstancesPath, s.capacityListWorkerInstances)
		r.Get(capacityapi.WorkerInstancesPath+"/{workerInstanceID}", s.capacityGetWorkerInstance)
		r.With(limitRequestBody(capacityRequestBodyLimit)).
			Post(capacityapi.WorkerInstancesPath+"/{workerInstanceID}/drain", s.capacityDrainWorkerInstance)
	})
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

func (s *Server) capacityObservations(w http.ResponseWriter, r *http.Request) {
	observations, err := workergroup.ObserveDemand(r.Context(), s.db)
	if err != nil {
		s.log.Error("observe deployment capacity demand", "error", err)
		writeError(w, errors.New("observe deployment capacity demand"))
		return
	}
	response := capacityapi.CapacityObservationsResponse{
		Observations: make([]capacityapi.CapacityObservation, 0, len(observations)),
	}
	for _, observation := range observations {
		projected, err := capacityCapacityObservation(observation)
		if err != nil {
			s.log.Error("project deployment capacity observation", "error", err)
			writeError(w, errors.New("project deployment capacity observation"))
			return
		}
		response.Observations = append(response.Observations, projected)
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
			row.ID, row.ResourceID, row.WorkerGroupID, row.State, row.ClaimVersion,
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
		row.ID, row.ResourceID, row.WorkerGroupID, row.State, row.ClaimVersion,
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
		draining.ID, draining.ResourceID, draining.WorkerGroupID, string(draining.State), draining.ClaimVersion,
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

func capacityCapacityObservation(observation workergroup.DemandObservation) (capacityapi.CapacityObservation, error) {
	groupStatus, err := workerGroupPublicStatus(observation.GroupState)
	if err != nil {
		return capacityapi.CapacityObservation{}, err
	}
	result := capacityapi.CapacityObservation{
		WorkerGroupID:      observation.WorkerGroupID,
		RegionID:           observation.RegionID,
		GroupStatus:        groupStatus,
		RegisteringWorkers: observation.RegisteringWorkers,
		DrainingWorkers:    observation.DrainingWorkers,
		ObservedAt:         observation.ObservedAt,
	}
	result.Run = capacityRoleDemand(observation.Run)
	result.Build = capacityRoleDemand(observation.Build)
	return result, nil
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

func capacityRoleDemand(role *workergroup.RoleDemand) *capacityapi.RoleDemand {
	if role == nil {
		return nil
	}
	return &capacityapi.RoleDemand{
		QueuedItems:       role.QueuedItems,
		QueuedResources:   capacityResourceVector(role.QueuedResources),
		ReadyWorkers:      role.ReadyWorkers,
		AvailableCapacity: capacityResourceVector(role.AvailableCapacity),
	}
}

func capacityResourceVector(vector workergroup.ResourceVector) capacityapi.ResourceVector {
	return capacityapi.ResourceVector{
		CPUMillis: vector.CPUMillis, MemoryBytes: vector.MemoryBytes,
		GuestEphemeralDiskBytes: vector.GuestEphemeralDiskBytes,
		VMSlots:                 vector.VMSlots, RunConsumers: vector.RunConsumers,
		BuildExecutors: vector.BuildExecutors,
	}
}

func capacityWorkerInstance(
	id pgtype.UUID,
	resourceID string,
	workerGroupID string,
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
		WorkerGroupID: workerGroupID, Status: publicStatus, ClaimVersion: claimVersion,
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
