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

func TestCapacityDrainUsesExactEpochAndClaimFence(t *testing.T) {
	workerID := pgvalue.NewUUIDv7()
	now := time.Now().UTC()
	store := &capacityDrainStore{
		instance: db.GetCapacityWorkerInstanceRow{
			ID: workerID, ResourceID: "host-opaque-1", WorkerGroupID: "run-workers",
			State: string(db.WorkerInstanceStateActive), ClaimVersion: 7,
			CurrentEpoch: pgtype.Int8{Int64: 4, Valid: true}, SupportsRun: true,
			CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
			UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		},
	}
	store.draining = db.DrainWorkerInstanceRow{
		ID: workerID, ResourceID: "host-opaque-1", WorkerGroupID: "run-workers",
		State: string(db.WorkerInstanceStateDraining), ClaimVersion: 8,
		CurrentEpoch: pgtype.Int8{Int64: 4, Valid: true}, SupportsRun: true,
		DrainingAt: pgtype.Timestamptz{Time: now, Valid: true},
		CreatedAt:  pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt:  pgtype.Timestamptz{Time: now, Valid: true},
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
	store := &capacityPlanStore{group: db.WorkerGroup{
		ID: groupID, RegionID: "aws-us-east-1", Name: "default", State: "active", AllowsRun: true,
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
	plan, err := client.Plan(t.Context(), groupID, capacityapi.CapacityPlanRequest{
		Worker: capacityHTTPManifest(t), MaxAdditionalWorkers: 2,
	})
	if err != nil || plan.WorkerGroupID != groupID || !plan.Complete || plan.RecommendedAdditionalWorkers != 0 {
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
	group db.WorkerGroup
}

func (s *capacityPlanStore) GetWorkerGroupByRegionName(context.Context, db.GetWorkerGroupByRegionNameParams) (db.WorkerGroup, error) {
	return s.group, nil
}

func (s *capacityPlanStore) GetWorkerGroup(context.Context, string) (db.WorkerGroup, error) {
	return s.group, nil
}

func (s *capacityPlanStore) ListWorkerCapacityBins(context.Context, db.ListWorkerCapacityBinsParams) ([]db.ListWorkerCapacityBinsRow, error) {
	return nil, nil
}

func (s *capacityPlanStore) ListQueuedRunCandidateScopes(context.Context, db.ListQueuedRunCandidateScopesParams) ([]db.ListQueuedRunCandidateScopesRow, error) {
	return nil, nil
}

func (s *capacityPlanStore) ListQueuedRunDispatchCandidatesForScope(context.Context, db.ListQueuedRunDispatchCandidatesForScopeParams) ([]db.ListQueuedRunDispatchCandidatesForScopeRow, error) {
	return nil, nil
}

func (s *capacityPlanStore) ListQueuedDeploymentBuildCandidates(context.Context, db.ListQueuedDeploymentBuildCandidatesParams) ([]db.ListQueuedDeploymentBuildCandidatesRow, error) {
	return nil, nil
}

func capacityHTTPManifest(t *testing.T) capacityapi.WorkerReleaseManifest {
	t.Helper()
	runtime := capacityapi.RuntimeProfile{
		Arch: "x86_64", Contract: "helmr.vm-runtime.v0",
		KernelDigest: "sha256:" + strings.Repeat("1", 64), InitramfsDigest: "sha256:" + strings.Repeat("2", 64),
		RootfsDigest: "sha256:" + strings.Repeat("3", 64),
	}
	runtime.ID, _ = runtime.ExpectedID()
	manifest := capacityapi.WorkerReleaseManifest{
		Schema: capacityapi.WorkerReleaseManifestSchema, WorkerVersion: "0123456789abcdef0123456789abcdef01234567", SupportsRun: true,
		Runtime: runtime, Substrate: capacityapi.SubstrateProfile{Format: "ext4", Contract: "helmr.substrate.ext4.v0"},
		Capacity: capacityapi.ResourceVector{CPUMillis: 2000, MemoryBytes: 2 << 30, GuestEphemeralDiskBytes: 64 << 30, VMSlots: 1},
		PerVM:    capacityapi.ResourceVector{CPUMillis: 2000, MemoryBytes: 2 << 30, GuestEphemeralDiskBytes: 32 << 30},
	}
	manifest.ReleaseFingerprint, _ = manifest.ExpectedFingerprint()
	return manifest
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
