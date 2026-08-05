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
	if result.Status != string(db.WorkerInstanceStateDraining) || result.ClaimVersion != 8 {
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
