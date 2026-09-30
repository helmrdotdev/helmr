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
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

const (
	capacityRequestBodyLimit      = int64(16 << 10)
	defaultCapacityInstanceLimit  = int32(200)
	maximumCapacityInstanceLimit  = int32(500)
	capacityTokenDecodedByteCount = 32
)

var capacityInstanceStatuses = map[workergroup.WorkerHostStatus]struct{}{
	workergroup.WorkerHostStatusRegistering:      {},
	workergroup.WorkerHostStatusActive:           {},
	workergroup.WorkerHostStatusDraining:         {},
	workergroup.WorkerHostStatusTerminationReady: {},
	workergroup.WorkerHostStatusLost:             {},
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
	r.Route("/capacity/v1", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(s.requireCapacity)
			r.Get("/worker-groups/resolve", s.capacityResolveWorkerGroup)
			r.Get("/worker-groups/{workerGroupID}/pools/resolve", s.capacityResolveWorkerPool)
			r.With(limitRequestBody(capacityRequestBodyLimit)).
				Put("/worker-groups/{workerGroupID}/primary-pools", s.capacityReconcileWorkerGroupPrimaryPools)
			r.With(limitRequestBody(capacityRequestBodyLimit)).
				Post("/worker-groups/{workerGroupID}/plan", s.capacityPlan)
			r.Get("/worker-hosts", s.capacityListWorkerHosts)
			r.Get("/worker-hosts/{workerHostID}", s.capacityGetWorkerHost)
			r.Post("/worker-hosts/{workerHostID}/lost", s.capacityConfirmWorkerHostProviderAbsent)
			r.With(limitRequestBody(capacityRequestBodyLimit)).
				Post("/worker-hosts/{workerHostID}/drain", s.capacityDrainWorkerHost)
		})
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
	group, err := workergroup.ResolveGroup(r.Context(), s.db, regionID, name)
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, group)
}

func (s *Server) capacityResolveWorkerPool(w http.ResponseWriter, r *http.Request) {
	workerGroupID, ok := capacityWorkerGroupID(w, r)
	if !ok {
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
	pool, err := workergroup.ResolvePool(r.Context(), s.db, workerGroupID, name)
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pool)
}

func (s *Server) capacityReconcileWorkerGroupPrimaryPools(w http.ResponseWriter, r *http.Request) {
	workerGroupID, ok := capacityWorkerGroupID(w, r)
	if !ok {
		return
	}
	var request workergroup.ReconcilePrimaryPoolsRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid primary Pool selection JSON: %w", err))
		return
	}
	if request.ExpectedGroupClaimVersion <= 0 {
		writeError(w, badRequest(errors.New("expected_group_claim_version must be positive")))
		return
	}
	poolID, err := capacityOptionalPoolID(request.PoolID)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("pool_id: %w", err)))
		return
	}
	response, err := workergroup.ReconcilePrimaryPools(r.Context(), s.tx, workerGroupID, poolID, request.ExpectedGroupClaimVersion)
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// capacityOptionalPoolID parses an empty or canonical pool ID; empty yields
// the zero ID, which primary selection rejects.
func capacityOptionalPoolID(raw string) (uuid.UUID, error) {
	if raw == "" {
		return uuid.Nil(), nil
	}
	id, err := ids.Parse(raw)
	if err != nil || id.String() != raw {
		return uuid.Nil(), errors.New("must be empty or a canonical UUIDv7")
	}
	return id, nil
}

func (s *Server) capacityPlan(w http.ResponseWriter, r *http.Request) {
	workerGroupID, ok := capacityWorkerGroupID(w, r)
	if !ok {
		return
	}
	var request workergroup.PlanRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid capacity plan JSON: %w", err))
		return
	}
	response, err := workergroup.Plan(r.Context(), s.db, workerGroupID, request, time.Now())
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) capacityListWorkerHosts(w http.ResponseWriter, r *http.Request) {
	filter, err := capacityWorkerHostFilter(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	response, err := workergroup.ListHosts(r.Context(), s.db, filter)
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) capacityGetWorkerHost(w http.ResponseWriter, r *http.Request) {
	id, err := capacityWorkerHostID(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	host, err := workergroup.GetHost(r.Context(), s.db, id)
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, host)
}

func (s *Server) capacityDrainWorkerHost(w http.ResponseWriter, r *http.Request) {
	id, err := capacityWorkerHostID(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var request workergroup.DrainWorkerHostRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker drain JSON: %w", err))
		return
	}
	host, err := workergroup.DrainHost(r.Context(), s.db, id, request)
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, host)
}

func (s *Server) capacityConfirmWorkerHostProviderAbsent(w http.ResponseWriter, r *http.Request) {
	id, err := capacityWorkerHostID(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if r.ContentLength != 0 {
		writeError(w, badRequest(errors.New("provider absence request must not contain a body")))
		return
	}
	host, err := workergroup.ConfirmHostProviderAbsent(r.Context(), s.db, s.tx, id)
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, host)
}

func capacityWorkerGroupID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := ids.Parse(chi.URLParam(r, "workerGroupID"))
	if err != nil {
		writeError(w, badRequest(errors.New("worker_group_id must be a canonical UUIDv7")))
		return uuid.Nil(), false
	}
	return id, true
}

func capacityWorkerHostID(r *http.Request) (uuid.UUID, error) {
	id, err := ids.Parse(chi.URLParam(r, "workerHostID"))
	if err != nil {
		return uuid.Nil(), errors.New("worker_host_id must be a canonical UUIDv7")
	}
	return id, nil
}

func capacityWorkerHostFilter(r *http.Request) (workergroup.HostFilter, error) {
	filter := workergroup.HostFilter{Limit: defaultCapacityInstanceLimit}
	query := r.URL.Query()
	for name := range query {
		switch name {
		case "worker_group_id", "resource_id", "status", "has_unreclaimed_runtime", "limit":
		default:
			return filter, fmt.Errorf("query parameter %q is not supported", name)
		}
	}
	if len(query["worker_group_id"]) > 1 || len(query["has_unreclaimed_runtime"]) > 1 || len(query["limit"]) > 1 {
		return filter, errors.New("worker_group_id, has_unreclaimed_runtime, and limit must not be repeated")
	}
	if groupIDs := query["worker_group_id"]; len(groupIDs) == 1 {
		parsed, err := ids.Parse(groupIDs[0])
		if err != nil {
			return filter, errors.New("worker_group_id must be a canonical UUIDv7")
		}
		filter.GroupID = parsed
	}
	if raw := strings.TrimSpace(query.Get("has_unreclaimed_runtime")); raw != "" {
		if raw != "true" {
			return filter, errors.New("has_unreclaimed_runtime must be true when present")
		}
		filter.HasUnreclaimedRuntime = true
	}
	for _, raw := range query["status"] {
		status := workergroup.WorkerHostStatus(strings.TrimSpace(raw))
		if _, ok := capacityInstanceStatuses[status]; !ok {
			return filter, fmt.Errorf("unsupported worker instance status %q", status)
		}
		filter.Statuses = append(filter.Statuses, status)
	}
	resourceIDs := map[string]struct{}{}
	for _, resourceID := range query["resource_id"] {
		resourceID = strings.TrimSpace(resourceID)
		if resourceID == "" || len(resourceID) > workergroup.MaxResourceIDBytes {
			return filter, fmt.Errorf("resource_id must contain between 1 and %d bytes", workergroup.MaxResourceIDBytes)
		}
		if _, duplicate := resourceIDs[resourceID]; duplicate {
			return filter, fmt.Errorf("resource_id %q is duplicated", resourceID)
		}
		resourceIDs[resourceID] = struct{}{}
		filter.ResourceIDs = append(filter.ResourceIDs, resourceID)
	}
	if len(filter.ResourceIDs) > int(maximumCapacityInstanceLimit) {
		return filter, fmt.Errorf("at most %d resource_id filters are allowed", maximumCapacityInstanceLimit)
	}
	if rawLimit := strings.TrimSpace(query.Get("limit")); rawLimit != "" {
		limit, err := strconv.ParseInt(rawLimit, 10, 32)
		if err != nil || limit <= 0 || limit > int64(maximumCapacityInstanceLimit) {
			return filter, fmt.Errorf("limit must be between 1 and %d", maximumCapacityInstanceLimit)
		}
		filter.Limit = int32(limit)
	}
	return filter, nil
}
