package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/capacityapi"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestHashCapacityTokenRequiresCanonicalHighEntropyValue(t *testing.T) {
	valid := capacityTestToken()
	if hash, err := hashCapacityToken(valid); err != nil || len(hash) == 0 {
		t.Fatalf("hash valid capacity token: hash=%x err=%v", hash, err)
	}
	for _, invalid := range []string{"short", valid + "=", " " + valid, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 31))} {
		if _, err := hashCapacityToken(invalid); err == nil {
			t.Fatalf("hashCapacityToken(%q) succeeded", invalid)
		}
	}
	if hash, err := hashCapacityToken(""); err != nil || hash != nil {
		t.Fatalf("empty optional token = %x, %v", hash, err)
	}
}

func TestCapacityRoutesRequireDedicatedBearer(t *testing.T) {
	hash, err := hashCapacityToken(capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{capacityTokenHash: hash}
	router := chi.NewRouter()
	router.Route("/api", server.mountCapacityRoutes)

	for name, authorization := range map[string]string{
		"missing":         "",
		"product token":   "Bearer hlmr_test_product",
		"malformed token": "Basic " + capacityTestToken(),
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/capacity/v0/worker-instances", nil)
			request.Header.Set("Authorization", authorization)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestCapacityPrimarySelectionIsAtomicAndReplaySafe(t *testing.T) {
	group, pool := adminPoolFixture()
	store := newAdminPoolStore(group, pool)
	store.switched = group
	store.switched.ClaimVersion++
	store.switched.PrimaryPoolID = pool.ID

	hash, err := hashCapacityToken(capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{db: store, capacityTokenHash: hash}
	router := chi.NewRouter()
	router.Route("/api", server.mountCapacityRoutes)
	httpServer := httptest.NewServer(router)
	defer httpServer.Close()
	client, err := capacityapi.NewClient(httpServer.URL, capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	poolID := pgvalue.UUIDString(pool.ID)
	response, err := client.ReconcileWorkerGroupPrimaryPools(t.Context(), group.ID, capacityapi.ReconcileWorkerGroupPrimaryPoolsRequest{
		ExpectedGroupClaimVersion: group.ClaimVersion,
		PoolID:                    poolID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Applied || response.WorkerGroup.ClaimVersion != group.ClaimVersion+1 ||
		response.WorkerGroup.PrimaryPoolID != poolID {
		t.Fatalf("response = %+v", response)
	}
	if store.switchCalls != 1 || store.switchParams.PoolID != pool.ID {
		t.Fatalf("set primary params = %+v, calls = %d", store.switchParams, store.switchCalls)
	}
	assertAdminPoolActions(t, store, "group", "pool", "switch")

	replayGroup := store.switched
	replayStore := newAdminPoolStore(replayGroup, pool)
	replayServer := &Server{db: replayStore, capacityTokenHash: hash}
	replayRouter := chi.NewRouter()
	replayRouter.Route("/api", replayServer.mountCapacityRoutes)
	replayHTTPServer := httptest.NewServer(replayRouter)
	defer replayHTTPServer.Close()
	replayClient, err := capacityapi.NewClient(replayHTTPServer.URL, capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := replayClient.ReconcileWorkerGroupPrimaryPools(t.Context(), group.ID, capacityapi.ReconcileWorkerGroupPrimaryPoolsRequest{
		ExpectedGroupClaimVersion: group.ClaimVersion,
		PoolID:                    poolID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Applied || replayed.WorkerGroup.ClaimVersion != replayGroup.ClaimVersion || replayStore.switchCalls != 0 {
		t.Fatalf("replay = %+v, set calls = %d", replayed, replayStore.switchCalls)
	}
}

func TestCapacityDrainUsesExactEpochAndClaimFence(t *testing.T) {
	workerID := pgvalue.NewUUIDv7()
	now := time.Now().UTC()
	store := &capacityDrainStore{
		instance: db.GetCapacityWorkerInstanceRow{
			ID: workerID, ResourceID: "host-opaque-1", WorkerGroupID: "run-workers",
			State: string(db.WorkerInstanceStateActive), ClaimVersion: 7,
			CurrentEpoch: pgtype.Int8{Int64: 4, Valid: true},
			CreatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
			UpdatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		},
	}
	store.draining = db.DrainWorkerInstanceRow{
		ID: workerID, ResourceID: "host-opaque-1", WorkerGroupID: "run-workers",
		State: string(db.WorkerInstanceStateDraining), ClaimVersion: 8,
		CurrentEpoch: pgtype.Int8{Int64: 4, Valid: true},
		DrainingAt:   pgtype.Timestamptz{Time: now, Valid: true},
		CreatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
	}
	hash, err := hashCapacityToken(capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{db: store, capacityTokenHash: hash}
	router := chi.NewRouter()
	router.Route("/api", server.mountCapacityRoutes)
	httpServer := httptest.NewServer(router)
	defer httpServer.Close()
	client, err := capacityapi.NewClient(httpServer.URL, capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.DrainWorkerInstance(t.Context(), uuid.UUID(workerID.Bytes).String(), capacityapi.DrainWorkerInstanceRequest{
		ExpectedEpoch: 4, ExpectedClaimVersion: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.params.ExpectedEpoch.Int64 != 4 || store.params.ExpectedClaimVersion != 7 || store.params.WorkerGroupID != "run-workers" {
		t.Fatalf("drain params = %+v", store.params)
	}
	if result.Status != capacityapi.WorkerInstanceStatusDraining || result.ClaimVersion != 8 {
		t.Fatalf("drain result = %+v", result)
	}
	replayed, err := client.DrainWorkerInstance(t.Context(), uuid.UUID(workerID.Bytes).String(), capacityapi.DrainWorkerInstanceRequest{
		ExpectedEpoch: 4, ExpectedClaimVersion: 7,
	})
	if err != nil || replayed.ID != result.ID || replayed.Status != result.Status ||
		replayed.ClaimVersion != result.ClaimVersion || replayed.CurrentEpoch == nil ||
		result.CurrentEpoch == nil || *replayed.CurrentEpoch != *result.CurrentEpoch {
		t.Fatalf("exact replay = %+v, %v", replayed, err)
	}
}

func TestCapacityClientDecodesStaleDrainConflict(t *testing.T) {
	workerID := pgvalue.NewUUIDv7()
	store := &capacityDrainStore{
		instance: db.GetCapacityWorkerInstanceRow{
			ID: workerID, ResourceID: "host-opaque-1", WorkerGroupID: "run-workers",
			State: string(db.WorkerInstanceStateActive), ClaimVersion: 7,
			CurrentEpoch: pgtype.Int8{Int64: 4, Valid: true},
		},
		drainErr: pgx.ErrNoRows,
	}
	hash, err := hashCapacityToken(capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Route("/api", (&Server{db: store, capacityTokenHash: hash}).mountCapacityRoutes)
	httpServer := httptest.NewServer(router)
	defer httpServer.Close()
	client, err := capacityapi.NewClient(httpServer.URL, capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.DrainWorkerInstance(t.Context(), uuid.UUID(workerID.Bytes).String(), capacityapi.DrainWorkerInstanceRequest{
		ExpectedEpoch: 4, ExpectedClaimVersion: 6,
	})
	var httpErr *capacityapi.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusConflict {
		t.Fatalf("stale drain error = %#v", err)
	}
}

func TestCapacityResolveAndPlanHandlers(t *testing.T) {
	groupID := uuid.Must(uuid.NewV7()).String()
	poolID := pgvalue.NewUUIDv7()
	template := capacityHTTPTemplate(t)
	store := &capacityPlanStore{group: db.WorkerGroup{
		ID: groupID, RegionID: "aws-us-east-1", Name: "default", State: "active",
	}, pool: db.WorkerPool{
		ID: poolID, WorkerGroupID: groupID, Name: "run-current", State: "active",
	}, planPool: db.ListCapacityWorkerPoolsRow{
		ID: poolID, WorkerGroupID: groupID, Name: "run-current",
		RuntimeIdentityID:               pgtype.Text{String: template.Runtime.ID, Valid: true},
		SubstrateFormat:                 pgtype.Text{String: template.Substrate.Format, Valid: true},
		SubstrateContract:               pgtype.Text{String: template.Substrate.Contract, Valid: true},
		CapacityCPUMillis:               pgtype.Int8{Int64: template.Capacity.CPUMillis, Valid: true},
		CapacityMemoryBytes:             pgtype.Int8{Int64: template.Capacity.MemoryBytes, Valid: true},
		CapacityGuestEphemeralDiskBytes: pgtype.Int8{Int64: template.Capacity.GuestEphemeralDiskBytes, Valid: true},
		PerVMCPUMillis:                  pgtype.Int8{Int64: template.PerVM.CPUMillis, Valid: true},
		PerVMMemoryBytes:                pgtype.Int8{Int64: template.PerVM.MemoryBytes, Valid: true},
		PerVMGuestEphemeralDiskBytes:    pgtype.Int8{Int64: template.PerVM.GuestEphemeralDiskBytes, Valid: true},
		MaxVMSlots:                      pgtype.Int4{Int32: int32(template.Capacity.VMSlots), Valid: true},
		CPUShapeVCPUCounts:              []int32{1, 2}, CPUShapeConfigDigests: []string{
			template.CPUShapes[0].CPUConfigDigest, template.CPUShapes[1].CPUConfigDigest,
		},
	}}
	hash, err := hashCapacityToken(capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Route("/api", (&Server{db: store, capacityTokenHash: hash}).mountCapacityRoutes)
	httpServer := httptest.NewServer(router)
	defer httpServer.Close()
	client, err := capacityapi.NewClient(httpServer.URL, capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := client.ResolveWorkerGroup(t.Context(), store.group.RegionID, store.group.Name)
	if err != nil || resolved.ID != groupID || resolved.Status != capacityapi.WorkerGroupStatusActive {
		t.Fatalf("resolved group = %+v, %v", resolved, err)
	}
	resolvedPool, err := client.ResolveWorkerPool(t.Context(), groupID, store.pool.Name)
	if err != nil || resolvedPool.ID != uuid.UUID(poolID.Bytes).String() || resolvedPool.Status != capacityapi.WorkerPoolStatusActive {
		t.Fatalf("resolved pool = %+v, %v", resolvedPool, err)
	}
	plan, err := client.Plan(t.Context(), groupID, capacityapi.CapacityPlanRequest{
		Pools: []capacityapi.CapacityPoolRequest{{
			PoolID: uuid.UUID(poolID.Bytes).String(), MaxAdditionalWorkers: 2,
		}},
	})
	if err != nil || plan.WorkerGroupID != groupID || !plan.Complete || len(plan.Pools) != 1 || plan.Pools[0].RecommendedAdditionalWorkers != 0 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}

	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/api/capacity/v0/worker-groups/resolve?region_id=aws-us-east-1&region_id=other&name=default"},
		{method: http.MethodPost, path: "/api/capacity/v0/worker-groups/not-a-group/plan", body: `{}`},
		{method: http.MethodPost, path: "/api/capacity/v0/worker-groups/" + groupID + "/plan", body: `{}`},
	} {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer "+capacityTestToken())
		if test.body != "" {
			request.Header.Set("Content-Type", "application/json")
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s %s status = %d, want 400: %s", test.method, test.path, response.Code, response.Body.String())
		}
	}
}

func TestCapacityWorkerInstanceListParamsAreBounded(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/?worker_group_id=run-workers&resource_id=host-1&resource_id=host-2&status=active&status=draining&limit=50", nil)
	params, err := capacityWorkerInstanceListParams(request)
	if err != nil {
		t.Fatal(err)
	}
	if !params.WorkerGroupID.Valid || params.WorkerGroupID.String != "run-workers" || params.RowLimit != 50 || strings.Join(params.ResourceIds, ",") != "host-1,host-2" || strings.Join(params.States, ",") != "active,draining" {
		t.Fatalf("params = %+v", params)
	}
	for _, raw := range []string{"/?unsupported=active", "/?status=unknown", "/?resource_id=", "/?resource_id=host-1&resource_id=host-1", "/?limit=0", "/?limit=501"} {
		if _, err := capacityWorkerInstanceListParams(httptest.NewRequest(http.MethodGet, raw, nil)); err == nil {
			t.Fatalf("params for %q succeeded", raw)
		}
	}
}

type capacityDrainStore struct {
	db.Querier
	instance db.GetCapacityWorkerInstanceRow
	draining db.DrainWorkerInstanceRow
	params   db.DrainWorkerInstanceParams
	drainErr error
}

type capacityPlanStore struct {
	db.Querier
	group    db.WorkerGroup
	pool     db.WorkerPool
	planPool db.ListCapacityWorkerPoolsRow
}

func (s *capacityPlanStore) GetWorkerGroupByRegionName(context.Context, db.GetWorkerGroupByRegionNameParams) (db.WorkerGroup, error) {
	return s.group, nil
}

func (s *capacityPlanStore) GetWorkerGroup(context.Context, string) (db.WorkerGroup, error) {
	return s.group, nil
}

func (s *capacityPlanStore) GetWorkerPoolByGroupName(context.Context, db.GetWorkerPoolByGroupNameParams) (db.WorkerPool, error) {
	return s.pool, nil
}

func (s *capacityPlanStore) ListCapacityWorkerPools(context.Context, db.ListCapacityWorkerPoolsParams) ([]db.ListCapacityWorkerPoolsRow, error) {
	return []db.ListCapacityWorkerPoolsRow{s.planPool}, nil
}

func (s *capacityPlanStore) ListWorkerCapacityBins(context.Context, db.ListWorkerCapacityBinsParams) ([]db.ListWorkerCapacityBinsRow, error) {
	return nil, nil
}

func (s *capacityPlanStore) ListQueuedRunEligibleScopes(context.Context, db.ListQueuedRunEligibleScopesParams) ([]db.ListQueuedRunEligibleScopesRow, error) {
	return nil, nil
}

func (s *capacityPlanStore) ListQueuedRunPlanningUsage(context.Context, db.ListQueuedRunPlanningUsageParams) ([]db.ListQueuedRunPlanningUsageRow, error) {
	return nil, nil
}

func (s *capacityPlanStore) ListQueuedRunPlanningCandidatesForScopes(context.Context, db.ListQueuedRunPlanningCandidatesForScopesParams) ([]db.ListQueuedRunPlanningCandidatesForScopesRow, error) {
	return nil, nil
}

func (s *capacityPlanStore) ListPendingWorkspaceExecCapacityCandidates(context.Context, db.ListPendingWorkspaceExecCapacityCandidatesParams) ([]db.ListPendingWorkspaceExecCapacityCandidatesRow, error) {
	return nil, nil
}

func capacityHTTPTemplate(t *testing.T) capacityapi.WorkerTemplate {
	t.Helper()
	runtime := capacityapi.RuntimeProfile{
		Arch: "x86_64", Contract: capacityapi.RuntimeContract,
		VMRuntimeDescriptorDigest: "sha256:" + strings.Repeat("a", 64),
		FirecrackerDigest:         "sha256:" + strings.Repeat("b", 64),
		FirecrackerVersion:        "1.16.1",
		SnapshotFormatVersion:     "6.0.0",
		HostKernelRelease:         "6.8.0-1024-aws",
		CPUTemplate:               capacityapi.CPUTemplateSelector{Kind: capacityapi.CPUTemplateNone},
		KernelDigest:              "sha256:" + strings.Repeat("1", 64),
		InitramfsDigest:           "sha256:" + strings.Repeat("2", 64),
		RootfsDigest:              "sha256:" + strings.Repeat("3", 64),
	}
	runtime.ID, _ = runtime.ExpectedID()
	template := capacityapi.WorkerTemplate{
		Schema:  capacityapi.WorkerTemplateSchema,
		Runtime: runtime,
		CPUShapes: []capacityapi.CPUShape{
			{VCPUCount: 1, CPUConfigDigest: "sha256:" + strings.Repeat("4", 64)},
			{VCPUCount: 2, CPUConfigDigest: "sha256:" + strings.Repeat("5", 64)},
		},
		Substrate: capacityapi.SubstrateProfile{Format: "ext4", Contract: "helmr.substrate.ext4.v0"},
		Capacity:  capacityapi.ResourceVector{CPUMillis: 2000, MemoryBytes: 2 << 30, GuestEphemeralDiskBytes: 64 << 30, VMSlots: 1},
		PerVM:     capacityapi.ResourceVector{CPUMillis: 2000, MemoryBytes: 2 << 30, GuestEphemeralDiskBytes: 32 << 30},
	}
	return template
}

func (s *capacityDrainStore) GetCapacityWorkerInstance(context.Context, pgtype.UUID) (db.GetCapacityWorkerInstanceRow, error) {
	return s.instance, nil
}

func (s *capacityDrainStore) DrainWorkerInstance(_ context.Context, params db.DrainWorkerInstanceParams) (db.DrainWorkerInstanceRow, error) {
	s.params = params
	return s.draining, s.drainErr
}

func capacityTestToken() string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, capacityTokenDecodedByteCount))
}
