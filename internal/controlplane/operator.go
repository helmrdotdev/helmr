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
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/helmrdotdev/helmr/operatorapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	operatorRequestBodyLimit      = int64(16 << 10)
	defaultOperatorInstanceLimit  = int32(200)
	maximumOperatorInstanceLimit  = int32(500)
	maximumOperatorResourceIDSize = 512
	operatorTokenDecodedByteCount = 32
)

var operatorInstanceStates = map[string]struct{}{
	string(db.WorkerInstanceStateRegistering):      {},
	string(db.WorkerInstanceStateActive):           {},
	string(db.WorkerInstanceStateDraining):         {},
	string(db.WorkerInstanceStateTerminationReady): {},
	string(db.WorkerInstanceStateLost):             {},
}

func hashOperatorToken(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) != operatorTokenDecodedByteCount || base64.RawURLEncoding.EncodeToString(decoded) != raw {
		return nil, errors.New("operator token must be a canonical base64url-no-pad encoding of exactly 32 bytes")
	}
	return auth.HashCredential(raw), nil
}

func (s *Server) mountOperatorRoutes(r chi.Router) {
	r.Route(operatorapi.RoutePrefix, func(r chi.Router) {
		r.Use(s.requireOperator)
		r.Get(operatorapi.CapacityObservationsPath, s.operatorCapacityObservations)
		r.Get(operatorapi.WorkerInstancesPath, s.operatorListWorkerInstances)
		r.Get(operatorapi.WorkerInstancesPath+"/{workerInstanceID}", s.operatorGetWorkerInstance)
		r.With(limitRequestBody(operatorRequestBodyLimit)).
			Post(operatorapi.WorkerInstancesPath+"/{workerInstanceID}/drain", s.operatorDrainWorkerInstance)
	})
}

func (s *Server) requireOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || len(s.operatorTokenHash) == 0 || !hmac.Equal(auth.HashCredential(raw), s.operatorTokenHash) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, unauthorized(errors.New("deployment operator authentication is required")))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) operatorCapacityObservations(w http.ResponseWriter, r *http.Request) {
	observations, err := workergroup.ObserveDemand(r.Context(), s.db)
	if err != nil {
		s.log.Error("observe deployment capacity demand", "error", err)
		writeError(w, errors.New("observe deployment capacity demand"))
		return
	}
	response := operatorapi.CapacityObservationsResponse{
		Observations: make([]operatorapi.CapacityObservation, 0, len(observations)),
	}
	for _, observation := range observations {
		response.Observations = append(response.Observations, operatorCapacityObservation(observation))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) operatorListWorkerInstances(w http.ResponseWriter, r *http.Request) {
	params, err := operatorWorkerInstanceListParams(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	rows, err := s.db.ListOperatorWorkerInstances(r.Context(), params)
	if err != nil {
		s.log.Error("list operator Worker instances", "error", err)
		writeError(w, errors.New("list operator Worker instances"))
		return
	}
	response := operatorapi.WorkerInstancesResponse{
		WorkerInstances: make([]operatorapi.WorkerInstance, 0, len(rows)),
	}
	for _, row := range rows {
		response.WorkerInstances = append(response.WorkerInstances, operatorWorkerInstance(
			row.ID, row.ResourceID, row.WorkerGroupID, row.State, row.ClaimVersion,
			row.CurrentEpoch, row.SupportsRun, row.SupportsBuild, row.DrainingAt,
			row.TerminationReadyAt, row.LostAt, row.CreatedAt, row.UpdatedAt,
		))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) operatorGetWorkerInstance(w http.ResponseWriter, r *http.Request) {
	id, err := operatorWorkerInstanceID(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	row, err := s.db.GetOperatorWorkerInstance(r.Context(), pgvalue.UUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("Worker instance was not found")))
		return
	}
	if err != nil {
		s.log.Error("get operator Worker instance", "worker_instance_id", id.String(), "error", err)
		writeError(w, errors.New("get operator Worker instance"))
		return
	}
	writeJSON(w, http.StatusOK, operatorWorkerInstance(
		row.ID, row.ResourceID, row.WorkerGroupID, row.State, row.ClaimVersion,
		row.CurrentEpoch, row.SupportsRun, row.SupportsBuild, row.DrainingAt,
		row.TerminationReadyAt, row.LostAt, row.CreatedAt, row.UpdatedAt,
	))
}

func (s *Server) operatorDrainWorkerInstance(w http.ResponseWriter, r *http.Request) {
	id, err := operatorWorkerInstanceID(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var request operatorapi.DrainWorkerInstanceRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Worker drain JSON: %w", err)))
		return
	}
	if request.ExpectedEpoch <= 0 || request.ExpectedClaimVersion <= 0 {
		writeError(w, badRequest(errors.New("expected_epoch and expected_claim_version must be positive")))
		return
	}
	instance, err := s.db.GetOperatorWorkerInstance(r.Context(), pgvalue.UUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("Worker instance was not found")))
		return
	}
	if err != nil {
		s.log.Error("get Worker instance for operator drain", "worker_instance_id", id.String(), "error", err)
		writeError(w, errors.New("get Worker instance for drain"))
		return
	}
	draining, err := s.db.DrainWorkerInstance(r.Context(), db.DrainWorkerInstanceParams{
		ID:                   pgvalue.UUID(id),
		WorkerGroupID:        instance.WorkerGroupID,
		ExpectedEpoch:        pgtype.Int8{Int64: request.ExpectedEpoch, Valid: true},
		ExpectedClaimVersion: request.ExpectedClaimVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("Worker drain fence is stale or the instance is not active")))
		return
	}
	if err != nil {
		s.log.Error("operator drain Worker instance", "worker_instance_id", id.String(), "error", err)
		writeError(w, errors.New("drain Worker instance"))
		return
	}
	writeJSON(w, http.StatusOK, operatorWorkerInstance(
		draining.ID, draining.ResourceID, draining.WorkerGroupID, string(draining.State), draining.ClaimVersion,
		draining.CurrentEpoch, draining.SupportsRun, draining.SupportsBuild, draining.DrainingAt,
		draining.TerminationReadyAt, draining.LostAt, draining.CreatedAt, draining.UpdatedAt,
	))
}

func operatorWorkerInstanceID(r *http.Request) (uuid.UUID, error) {
	id, err := ids.Parse(chi.URLParam(r, "workerInstanceID"))
	if err != nil {
		return uuid.Nil, errors.New("worker_instance_id must be a canonical UUIDv7")
	}
	return id, nil
}

func operatorWorkerInstanceListParams(r *http.Request) (db.ListOperatorWorkerInstancesParams, error) {
	params := db.ListOperatorWorkerInstancesParams{
		ResourceIds: []string{},
		States:      []string{},
		RowLimit:    defaultOperatorInstanceLimit,
	}
	if groupID := strings.TrimSpace(r.URL.Query().Get("worker_group_id")); groupID != "" {
		params.WorkerGroupID = pgtype.Text{String: groupID, Valid: true}
	}
	for _, state := range r.URL.Query()["state"] {
		state = strings.TrimSpace(state)
		if _, ok := operatorInstanceStates[state]; !ok {
			return params, fmt.Errorf("unsupported Worker instance state %q", state)
		}
		params.States = append(params.States, state)
	}
	resourceIDs := map[string]struct{}{}
	for _, resourceID := range r.URL.Query()["resource_id"] {
		resourceID = strings.TrimSpace(resourceID)
		if resourceID == "" || len(resourceID) > maximumOperatorResourceIDSize {
			return params, fmt.Errorf("resource_id must contain between 1 and %d bytes", maximumOperatorResourceIDSize)
		}
		if _, duplicate := resourceIDs[resourceID]; duplicate {
			return params, fmt.Errorf("resource_id %q is duplicated", resourceID)
		}
		resourceIDs[resourceID] = struct{}{}
		params.ResourceIds = append(params.ResourceIds, resourceID)
	}
	if len(params.ResourceIds) > int(maximumOperatorInstanceLimit) {
		return params, fmt.Errorf("at most %d resource_id filters are allowed", maximumOperatorInstanceLimit)
	}
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		limit, err := strconv.ParseInt(rawLimit, 10, 32)
		if err != nil || limit <= 0 || limit > int64(maximumOperatorInstanceLimit) {
			return params, fmt.Errorf("limit must be between 1 and %d", maximumOperatorInstanceLimit)
		}
		params.RowLimit = int32(limit)
	}
	return params, nil
}

func operatorCapacityObservation(observation workergroup.DemandObservation) operatorapi.CapacityObservation {
	result := operatorapi.CapacityObservation{
		WorkerGroupID:      observation.WorkerGroupID,
		RegionID:           observation.RegionID,
		GroupState:         observation.GroupState,
		RegisteringWorkers: observation.RegisteringWorkers,
		DrainingWorkers:    observation.DrainingWorkers,
		ObservedAt:         observation.ObservedAt,
	}
	result.Run = operatorRoleDemand(observation.Run)
	result.Build = operatorRoleDemand(observation.Build)
	return result
}

func operatorRoleDemand(role *workergroup.RoleDemand) *operatorapi.RoleDemand {
	if role == nil {
		return nil
	}
	return &operatorapi.RoleDemand{
		QueuedItems:       role.QueuedItems,
		QueuedResources:   operatorResourceVector(role.QueuedResources),
		ReadyWorkers:      role.ReadyWorkers,
		AvailableCapacity: operatorResourceVector(role.AvailableCapacity),
	}
}

func operatorResourceVector(vector workergroup.ResourceVector) operatorapi.ResourceVector {
	return operatorapi.ResourceVector{
		CPUMillis: vector.CPUMillis, MemoryBytes: vector.MemoryBytes,
		GuestEphemeralDiskBytes: vector.GuestEphemeralDiskBytes,
		VMSlots:                 vector.VMSlots, RunConsumers: vector.RunConsumers,
		BuildExecutors: vector.BuildExecutors,
	}
}

func operatorWorkerInstance(
	id pgtype.UUID,
	resourceID string,
	workerGroupID string,
	state string,
	claimVersion int64,
	currentEpoch pgtype.Int8,
	supportsRun bool,
	supportsBuild bool,
	drainingAt pgtype.Timestamptz,
	terminationReadyAt pgtype.Timestamptz,
	lostAt pgtype.Timestamptz,
	createdAt pgtype.Timestamptz,
	updatedAt pgtype.Timestamptz,
) operatorapi.WorkerInstance {
	result := operatorapi.WorkerInstance{
		ID: uuid.UUID(id.Bytes).String(), ResourceID: resourceID,
		WorkerGroupID: workerGroupID, State: state, ClaimVersion: claimVersion,
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
	return result
}
