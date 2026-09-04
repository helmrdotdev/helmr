package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/runtimeid"
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
	server.mountCapacityRoutes(router)

	for name, authorization := range map[string]string{
		"missing":         "",
		"product token":   "Bearer hlmr_test_product",
		"malformed token": "Basic " + capacityTestToken(),
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/capacity/v1/worker-instances", nil)
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
	server.mountCapacityRoutes(router)
	poolID := pgvalue.UUIDString(pool.ID)
	response := capacityJSON(t, capacityRequest(t, router, http.MethodPut,
		"/capacity/v1/worker-groups/"+pgvalue.UUIDString(group.ID)+"/primary-pools",
		fmt.Sprintf(`{"expected_group_claim_version":%d,"pool_id":%q}`, group.ClaimVersion, poolID),
	), http.StatusOK)
	assertCapacityJSONKeys(t, response, "applied", "worker_group")
	responseGroup := capacityJSONObject(t, response["worker_group"])
	assertCapacityJSONKeys(t, responseGroup, "claim_version", "id", "name", "primary_pool_id", "region_id", "status")
	if response["applied"] != true || responseGroup["claim_version"] != float64(group.ClaimVersion+1) ||
		responseGroup["primary_pool_id"] != poolID {
		t.Fatalf("response = %#v", response)
	}
	if store.switchCalls != 1 || store.switchParams.PoolID != pool.ID {
		t.Fatalf("set primary params = %+v, calls = %d", store.switchParams, store.switchCalls)
	}
	assertAdminPoolActions(t, store, "group", "pool", "switch")

	replayGroup := store.switched
	replayStore := newAdminPoolStore(replayGroup, pool)
	replayServer := &Server{db: replayStore, capacityTokenHash: hash}
	replayRouter := chi.NewRouter()
	replayServer.mountCapacityRoutes(replayRouter)
	replayed := capacityJSON(t, capacityRequest(t, replayRouter, http.MethodPut,
		"/capacity/v1/worker-groups/"+pgvalue.UUIDString(group.ID)+"/primary-pools",
		fmt.Sprintf(`{"expected_group_claim_version":%d,"pool_id":%q}`, group.ClaimVersion, poolID),
	), http.StatusOK)
	replayedGroup := capacityJSONObject(t, replayed["worker_group"])
	if replayed["applied"] != false || replayedGroup["claim_version"] != float64(replayGroup.ClaimVersion) || replayStore.switchCalls != 0 {
		t.Fatalf("replay = %#v, set calls = %d", replayed, replayStore.switchCalls)
	}
}

func TestCapacityDrainUsesExactEpochAndClaimFence(t *testing.T) {
	workerID := pgvalue.NewUUIDv7()
	now := time.Now().UTC()
	store := &capacityDrainStore{
		instance: db.GetCapacityWorkerInstanceRow{
			ID: workerID, ResourceID: "host-opaque-1", WorkerGroupID: controlplaneTestWorkerGroupDBID,
			State: string(db.WorkerInstanceStateActive), ClaimVersion: 7,
			CurrentEpoch: pgtype.Int8{Int64: 4, Valid: true},
			CreatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
			UpdatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		},
		group: db.WorkerGroup{ID: controlplaneTestWorkerGroupDBID, RegionID: "us-east-1"},
	}
	store.draining = db.DrainWorkerInstanceRow{
		ID: workerID, ResourceID: "host-opaque-1", WorkerGroupID: controlplaneTestWorkerGroupDBID,
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
	server.mountCapacityRoutes(router)
	path := "/capacity/v1/worker-instances/" + uuid.UUID(workerID.Bytes).String() + "/drain"
	result := capacityJSON(t, capacityRequest(t, router, http.MethodPost, path,
		`{"expected_epoch":4,"expected_claim_version":7,"require_zero_queued_demand":true}`,
	), http.StatusOK)
	assertCapacityJSONKeys(t, result, "claim_version", "created_at", "current_epoch", "draining_at", "id", "resource_id", "status", "updated_at", "worker_group_id", "worker_pool_id")
	if store.params.ExpectedEpoch.Int64 != 4 || store.params.ExpectedClaimVersion != 7 || store.params.WorkerGroupID != controlplaneTestWorkerGroupDBID {
		t.Fatalf("drain params = %+v", store.params)
	}
	if result["status"] != "draining" || result["claim_version"] != float64(8) || result["current_epoch"] != float64(4) {
		t.Fatalf("drain result = %#v", result)
	}
	replayed := capacityJSON(t, capacityRequest(t, router, http.MethodPost, path,
		`{"expected_epoch":4,"expected_claim_version":7,"require_zero_queued_demand":true}`,
	), http.StatusOK)
	if !reflect.DeepEqual(replayed, result) {
		t.Fatalf("exact replay = %#v, want %#v", replayed, result)
	}
	if store.groupCalls != 2 || store.queuedRunCalls != 2 || store.queuedExecCalls != 2 || store.drainCalls != 2 {
		t.Fatalf("demand/drain calls = group:%d run:%d exec:%d drain:%d", store.groupCalls, store.queuedRunCalls, store.queuedExecCalls, store.drainCalls)
	}
}

func TestCapacityDrainDefersForEligibleQueuedDemand(t *testing.T) {
	workerID := pgvalue.NewUUIDv7()
	store := &capacityDrainStore{
		instance: db.GetCapacityWorkerInstanceRow{
			ID: workerID, WorkerGroupID: controlplaneTestWorkerGroupDBID, State: string(db.WorkerInstanceStateActive),
			ClaimVersion: 7, CurrentEpoch: pgtype.Int8{Int64: 4, Valid: true},
		},
		group:      db.WorkerGroup{ID: controlplaneTestWorkerGroupDBID, RegionID: "us-east-1"},
		queuedRuns: []db.ListQueuedRunEligibleScopesRow{{RegionID: "us-east-1"}},
	}
	hash, err := hashCapacityToken(capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	(&Server{db: store, capacityTokenHash: hash}).mountCapacityRoutes(router)
	response := capacityJSON(t, capacityRequest(t, router, http.MethodPost,
		"/capacity/v1/worker-instances/"+uuid.UUID(workerID.Bytes).String()+"/drain",
		`{"expected_epoch":4,"expected_claim_version":7,"require_zero_queued_demand":true}`,
	), http.StatusConflict)
	assertCapacityJSONKeys(t, response, "error")
	errorObject := capacityJSONObject(t, response["error"])
	assertCapacityJSONKeys(t, errorObject, "code", "message")
	if errorObject["code"] != "queued_demand_present" {
		t.Fatalf("queued demand error = %#v", response)
	}
	if store.drainCalls != 0 || store.queuedExecCalls != 0 {
		t.Fatalf("drain calls = %d, Workspace Exec queries = %d", store.drainCalls, store.queuedExecCalls)
	}
}

func TestCapacityDrainReplaySkipsQueuedDemandCheck(t *testing.T) {
	workerID := pgvalue.NewUUIDv7()
	store := &capacityDrainStore{
		instance: db.GetCapacityWorkerInstanceRow{
			ID: workerID, WorkerGroupID: controlplaneTestWorkerGroupDBID, State: string(db.WorkerInstanceStateDraining),
			ClaimVersion: 8, CurrentEpoch: pgtype.Int8{Int64: 4, Valid: true},
		},
		draining: db.DrainWorkerInstanceRow{
			ID: workerID, WorkerGroupID: controlplaneTestWorkerGroupDBID, State: string(db.WorkerInstanceStateDraining),
			ClaimVersion: 8, CurrentEpoch: pgtype.Int8{Int64: 4, Valid: true},
		},
	}
	hash, err := hashCapacityToken(capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	(&Server{db: store, capacityTokenHash: hash}).mountCapacityRoutes(router)
	result := capacityJSON(t, capacityRequest(t, router, http.MethodPost,
		"/capacity/v1/worker-instances/"+uuid.UUID(workerID.Bytes).String()+"/drain",
		`{"expected_epoch":4,"expected_claim_version":7,"require_zero_queued_demand":true}`,
	), http.StatusOK)
	if result["status"] != "draining" {
		t.Fatalf("drain replay = %#v", result)
	}
	if store.groupCalls != 0 || store.drainCalls != 1 {
		t.Fatalf("group calls = %d, drain calls = %d", store.groupCalls, store.drainCalls)
	}
}

func TestCapacityStaleDrainReturnsConflict(t *testing.T) {
	workerID := pgvalue.NewUUIDv7()
	store := &capacityDrainStore{
		instance: db.GetCapacityWorkerInstanceRow{
			ID: workerID, ResourceID: "host-opaque-1", WorkerGroupID: controlplaneTestWorkerGroupDBID,
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
	(&Server{db: store, capacityTokenHash: hash}).mountCapacityRoutes(router)
	response := capacityJSON(t, capacityRequest(t, router, http.MethodPost,
		"/capacity/v1/worker-instances/"+uuid.UUID(workerID.Bytes).String()+"/drain",
		`{"expected_epoch":4,"expected_claim_version":6}`,
	), http.StatusConflict)
	errorObject := capacityJSONObject(t, response["error"])
	assertCapacityJSONKeys(t, errorObject, "code", "message")
	if errorObject["code"] != "conflict" {
		t.Fatalf("stale drain error = %#v", response)
	}
	if store.groupCalls != 0 || store.drainCalls != 1 {
		t.Fatalf("ungated drain calls = group:%d drain:%d", store.groupCalls, store.drainCalls)
	}
}

func TestCapacityProviderAbsenceUsesExactWorkerIdentity(t *testing.T) {
	workerID := pgvalue.NewUUIDv7()
	now := time.Now().UTC()
	poolID := pgvalue.NewUUIDv7()
	store := &capacityDrainStore{
		instance: db.GetCapacityWorkerInstanceRow{
			ID: workerID, ResourceID: "i-provider-absent", WorkerGroupID: controlplaneTestWorkerGroupDBID,
			WorkerPoolID: poolID, State: string(db.WorkerInstanceStateActive), ClaimVersion: 7,
			CurrentEpoch: pgtype.Int8{Int64: 4, Valid: true},
			CreatedAt:    pgtype.Timestamptz{Time: now, Valid: true}, UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		},
		providerAbsent: db.ConfirmWorkerInstanceProviderAbsentRow{
			ID: workerID, ResourceID: "i-provider-absent", WorkerGroupID: controlplaneTestWorkerGroupDBID,
			WorkerPoolID: poolID, State: string(db.WorkerInstanceStateLost), ClaimVersion: 8,
			CurrentEpoch: pgtype.Int8{Int64: 4, Valid: true}, LostAt: pgtype.Timestamptz{Time: now, Valid: true},
			CreatedAt: pgtype.Timestamptz{Time: now, Valid: true}, UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		},
	}
	hash, err := hashCapacityToken(capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{db: store, capacityTokenHash: hash}
	router := chi.NewRouter()
	server.mountCapacityRoutes(router)
	result := capacityJSON(t, capacityRequest(t, router, http.MethodPost,
		"/capacity/v1/worker-instances/"+uuid.UUID(workerID.Bytes).String()+"/lost", "",
	), http.StatusOK)
	assertCapacityJSONKeys(t, result, "claim_version", "created_at", "current_epoch", "id", "lost_at", "resource_id", "status", "updated_at", "worker_group_id", "worker_pool_id")
	if store.providerAbsentID != workerID || result["status"] != "lost" || result["claim_version"] != float64(8) || result["resource_id"] != "i-provider-absent" {
		t.Fatalf("provider absence = id:%v result:%#v", store.providerAbsentID, result)
	}
}

func TestCapacityResolveAndPlanHandlers(t *testing.T) {
	groupID := uuid.NewV7().String()
	groupDBID := pgvalue.UUID(uuid.MustParse(groupID))
	poolID := pgvalue.NewUUIDv7()
	template := capacityHTTPTemplate(t)
	store := &capacityPlanStore{group: db.WorkerGroup{
		ID: groupDBID, RegionID: "aws-us-east-1", Name: "default", State: "active",
	}, pool: db.WorkerPool{
		ID: poolID, WorkerGroupID: groupDBID, Name: "run-current", State: "active",
	}, planPool: db.ListCapacityWorkerPoolsRow{
		ID: poolID, WorkerGroupID: groupDBID, Name: "run-current",
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
	(&Server{db: store, capacityTokenHash: hash}).mountCapacityRoutes(router)
	resolved := capacityJSON(t, capacityRequest(t, router, http.MethodGet,
		"/capacity/v1/worker-groups/resolve?region_id=aws-us-east-1&name=default", "",
	), http.StatusOK)
	assertCapacityJSONKeys(t, resolved, "claim_version", "id", "name", "region_id", "status")
	if resolved["id"] != groupID || resolved["status"] != "active" {
		t.Fatalf("resolved group = %#v", resolved)
	}
	resolvedPool := capacityJSON(t, capacityRequest(t, router, http.MethodGet,
		"/capacity/v1/worker-groups/"+groupID+"/pools/resolve?name=run-current", "",
	), http.StatusOK)
	assertCapacityJSONKeys(t, resolvedPool, "id", "name", "status", "worker_group_id")
	if resolvedPool["id"] != uuid.UUID(poolID.Bytes).String() || resolvedPool["status"] != "active" {
		t.Fatalf("resolved pool = %#v", resolvedPool)
	}
	plan := capacityJSON(t, capacityRequest(t, router, http.MethodPost,
		"/capacity/v1/worker-groups/"+groupID+"/plan",
		fmt.Sprintf(`{"pools":[{"pool_id":%q,"max_additional_workers":2}]}`, uuid.UUID(poolID.Bytes).String()),
	), http.StatusOK)
	assertCapacityJSONKeys(t, plan, "complete", "computed_at", "group_status", "pools", "region_id", "unmatched_demand", "worker_group_id", "worker_group_name")
	pools, ok := plan["pools"].([]any)
	if !ok || len(pools) != 1 {
		t.Fatalf("plan pools = %#v", plan["pools"])
	}
	poolPlan := capacityJSONObject(t, pools[0])
	assertCapacityJSONKeys(t, poolPlan, "active_workers", "complete", "compatible_queued_items", "pool_id", "pool_name", "recommended_additional_workers", "registering_workers", "saturated", "scale_in_blocked")
	if plan["worker_group_id"] != groupID || plan["complete"] != true || poolPlan["recommended_additional_workers"] != float64(0) {
		t.Fatalf("plan = %#v", plan)
	}

	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/capacity/v1/worker-groups/resolve?region_id=aws-us-east-1&region_id=other&name=default"},
		{method: http.MethodGet, path: "/capacity/v1/worker-instances?worker_group_id=not-a-group"},
		{method: http.MethodGet, path: "/capacity/v1/worker-instances?worker_group_id=%20"},
		{method: http.MethodGet, path: "/capacity/v1/worker-instances?worker_group_id=%20" + groupID + "%20"},
		{method: http.MethodPost, path: "/capacity/v1/worker-groups/not-a-group/plan", body: `{}`},
		{method: http.MethodPost, path: "/capacity/v1/worker-groups/" + groupID + "/plan", body: `{}`},
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
	request := httptest.NewRequest(http.MethodGet, "/?worker_group_id="+controlplaneTestWorkerGroup+"&resource_id=host-1&resource_id=host-2&status=active&status=draining&has_unreclaimed_runtime=true&limit=50", nil)
	params, err := capacityWorkerInstanceListParams(request)
	if err != nil {
		t.Fatal(err)
	}
	if !params.WorkerGroupID.Valid || pgvalue.UUIDString(params.WorkerGroupID) != controlplaneTestWorkerGroup || !params.HasUnreclaimedRuntime || params.RowLimit != 50 || strings.Join(params.ResourceIds, ",") != "host-1,host-2" || strings.Join(params.States, ",") != "active,draining" {
		t.Fatalf("params = %+v", params)
	}
	for _, raw := range []string{"/?unsupported=active", "/?worker_group_id=", "/?worker_group_id=%20", "/?worker_group_id=%20" + controlplaneTestWorkerGroup + "%20", "/?worker_group_id=run-workers", "/?status=unknown", "/?resource_id=", "/?resource_id=host-1&resource_id=host-1", "/?has_unreclaimed_runtime=false", "/?has_unreclaimed_runtime=true&has_unreclaimed_runtime=true", "/?limit=0", "/?limit=501"} {
		if _, err := capacityWorkerInstanceListParams(httptest.NewRequest(http.MethodGet, raw, nil)); err == nil {
			t.Fatalf("params for %q succeeded", raw)
		}
	}
}

func TestCapacityWorkerInstanceReadContract(t *testing.T) {
	workerID := pgvalue.NewUUIDv7()
	groupID := uuid.NewV7().String()
	groupDBID := pgvalue.UUID(uuid.MustParse(groupID))
	poolID := pgvalue.NewUUIDv7()
	now := time.Now().UTC()
	row := db.ListCapacityWorkerInstancesRow{
		ID: workerID, ResourceID: "host-opaque-1", WorkerGroupID: groupDBID, WorkerPoolID: poolID,
		State: string(db.WorkerInstanceStateActive), ClaimVersion: 7,
		CurrentEpoch: pgtype.Int8{Int64: 4, Valid: true},
		CreatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
	}
	store := &capacityDrainStore{
		instance: db.GetCapacityWorkerInstanceRow{
			ID: row.ID, ResourceID: row.ResourceID, WorkerGroupID: row.WorkerGroupID,
			WorkerPoolID: row.WorkerPoolID, State: row.State, ClaimVersion: row.ClaimVersion, CurrentEpoch: row.CurrentEpoch,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		},
		listed: []db.ListCapacityWorkerInstancesRow{row},
	}
	hash, err := hashCapacityToken(capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	(&Server{db: store, capacityTokenHash: hash}).mountCapacityRoutes(router)

	list := capacityJSON(t, capacityRequest(t, router, http.MethodGet,
		"/capacity/v1/worker-instances?worker_group_id="+groupID+"&status=active&limit=1", "",
	), http.StatusOK)
	assertCapacityJSONKeys(t, list, "worker_instances")
	instances, ok := list["worker_instances"].([]any)
	if !ok || len(instances) != 1 {
		t.Fatalf("worker_instances = %#v", list["worker_instances"])
	}
	listed := capacityJSONObject(t, instances[0])
	assertCapacityJSONKeys(t, listed, "claim_version", "created_at", "current_epoch", "id", "resource_id", "status", "updated_at", "worker_group_id", "worker_pool_id")
	if pgvalue.UUIDString(store.listParams.WorkerGroupID) != groupID || strings.Join(store.listParams.States, ",") != "active" || store.listParams.RowLimit != 1 {
		t.Fatalf("list params = %+v", store.listParams)
	}
	if listed["id"] != uuid.UUID(workerID.Bytes).String() || listed["resource_id"] != row.ResourceID ||
		listed["worker_group_id"] != groupID || listed["worker_pool_id"] != uuid.UUID(poolID.Bytes).String() ||
		listed["status"] != "active" || listed["claim_version"] != float64(7) || listed["current_epoch"] != float64(4) {
		t.Fatalf("listed instance = %#v", listed)
	}

	got := capacityJSON(t, capacityRequest(t, router, http.MethodGet,
		"/capacity/v1/worker-instances/"+uuid.UUID(workerID.Bytes).String(), "",
	), http.StatusOK)
	assertCapacityJSONKeys(t, got, "claim_version", "created_at", "current_epoch", "id", "resource_id", "status", "updated_at", "worker_group_id", "worker_pool_id")
	if !reflect.DeepEqual(got, listed) {
		t.Fatalf("get = %#v, want listed instance %#v", got, listed)
	}
}

type capacityDrainStore struct {
	db.Querier
	instance          db.GetCapacityWorkerInstanceRow
	listed            []db.ListCapacityWorkerInstancesRow
	listParams        db.ListCapacityWorkerInstancesParams
	group             db.WorkerGroup
	draining          db.DrainWorkerInstanceRow
	queuedRuns        []db.ListQueuedRunEligibleScopesRow
	queuedExecs       []db.ListPendingWorkspaceExecCapacityCandidatesRow
	providerAbsent    db.ConfirmWorkerInstanceProviderAbsentRow
	providerAbsentID  pgtype.UUID
	params            db.DrainWorkerInstanceParams
	drainErr          error
	providerAbsentErr error
	groupCalls        int
	queuedRunCalls    int
	queuedExecCalls   int
	drainCalls        int
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

func (s *capacityPlanStore) GetWorkerGroup(context.Context, pgtype.UUID) (db.WorkerGroup, error) {
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

func capacityHTTPTemplate(t *testing.T) capacity.WorkerTemplate {
	t.Helper()
	runtime := runtimeid.Profile{
		Arch: "x86_64", Contract: runtimeid.Contract,
		VMRuntimeDescriptorDigest: "sha256:" + strings.Repeat("a", 64),
		FirecrackerDigest:         "sha256:" + strings.Repeat("b", 64),
		FirecrackerVersion:        "1.16.1",
		SnapshotFormatVersion:     "6.0.0",
		HostKernelRelease:         "6.8.0-1024-aws",
		CPUTemplate:               runtimeid.CPUTemplateSelector{Kind: runtimeid.CPUTemplateNone},
		KernelDigest:              "sha256:" + strings.Repeat("1", 64),
		InitramfsDigest:           "sha256:" + strings.Repeat("2", 64),
		RootfsDigest:              "sha256:" + strings.Repeat("3", 64),
	}
	runtime.ID, _ = runtime.ExpectedID()
	template := capacity.WorkerTemplate{
		Schema:  capacity.WorkerTemplateSchema,
		Runtime: runtime,
		CPUShapes: []runtimeid.CPUShape{
			{VCPUCount: 1, CPUConfigDigest: "sha256:" + strings.Repeat("4", 64)},
			{VCPUCount: 2, CPUConfigDigest: "sha256:" + strings.Repeat("5", 64)},
		},
		Substrate: capacity.SubstrateProfile{Format: "ext4", Contract: "helmr.substrate.ext4.v0"},
		Capacity:  capacity.ResourceVector{CPUMillis: 2000, MemoryBytes: 2 << 30, GuestEphemeralDiskBytes: 64 << 30, VMSlots: 1},
		PerVM:     capacity.ResourceVector{CPUMillis: 2000, MemoryBytes: 2 << 30, GuestEphemeralDiskBytes: 32 << 30},
	}
	return template
}

func (s *capacityDrainStore) GetCapacityWorkerInstance(context.Context, pgtype.UUID) (db.GetCapacityWorkerInstanceRow, error) {
	return s.instance, nil
}

func (s *capacityDrainStore) ListCapacityWorkerInstances(_ context.Context, params db.ListCapacityWorkerInstancesParams) ([]db.ListCapacityWorkerInstancesRow, error) {
	s.listParams = params
	return s.listed, nil
}

func (s *capacityDrainStore) GetWorkerGroup(context.Context, pgtype.UUID) (db.WorkerGroup, error) {
	s.groupCalls++
	return s.group, nil
}

func (s *capacityDrainStore) ListQueuedRunEligibleScopes(context.Context, db.ListQueuedRunEligibleScopesParams) ([]db.ListQueuedRunEligibleScopesRow, error) {
	s.queuedRunCalls++
	return s.queuedRuns, nil
}

func (s *capacityDrainStore) ListPendingWorkspaceExecCapacityCandidates(context.Context, db.ListPendingWorkspaceExecCapacityCandidatesParams) ([]db.ListPendingWorkspaceExecCapacityCandidatesRow, error) {
	s.queuedExecCalls++
	return s.queuedExecs, nil
}

func (s *capacityDrainStore) BeginQuerier(context.Context) (db.Querier, transaction, error) {
	return s, &adminHTTPTransaction{}, nil
}

func (s *capacityDrainStore) DrainWorkerInstance(_ context.Context, params db.DrainWorkerInstanceParams) (db.DrainWorkerInstanceRow, error) {
	s.drainCalls++
	s.params = params
	return s.draining, s.drainErr
}

func (s *capacityDrainStore) ConfirmWorkerInstanceProviderAbsent(_ context.Context, id pgtype.UUID) (db.ConfirmWorkerInstanceProviderAbsentRow, error) {
	s.providerAbsentID = id
	return s.providerAbsent, s.providerAbsentErr
}

func (s *capacityDrainStore) ReconcileProviderAbsentWorkerRuntimes(context.Context, pgtype.UUID) (int64, error) {
	return 0, nil
}

func capacityTestToken() string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, capacityTokenDecodedByteCount))
}

func capacityRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+capacityTestToken())
	request.Header.Set("Accept", "application/json")
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func capacityJSON(t *testing.T, response *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d: %s", response.Code, status, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response JSON: %v: %s", err, response.Body.String())
	}
	return result
}

func capacityJSONObject(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("JSON value = %#v, want object", value)
	}
	return result
}

func assertCapacityJSONKeys(t *testing.T, object map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON keys = %v, want %v", got, want)
	}
}
