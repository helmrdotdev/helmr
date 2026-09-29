package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type adminPoolStore struct {
	*adminHTTPQuerier
	group              db.WorkerGroup
	pool               db.WorkerPool
	pools              []db.WorkerPool
	created            db.WorkerPool
	switched           db.WorkerGroup
	transitioned       db.WorkerPool
	createParams       db.CreatePendingWorkerPoolParams
	switchParams       db.SetWorkerGroupPrimaryPoolParams
	transitionParams   db.TransitionWorkerPoolLifecycleParams
	createCalls        int
	switchCalls        int
	transitionCalls    int
	transitionErr      error
	transactionActions []string
}

func newAdminPoolStore(group db.WorkerGroup, pool db.WorkerPool) *adminPoolStore {
	return &adminPoolStore{
		adminHTTPQuerier: &adminHTTPQuerier{admin: true},
		group:            group,
		pool:             pool,
		pools:            []db.WorkerPool{pool},
	}
}

func (s *adminPoolStore) GetWorkerGroup(context.Context, pgtype.UUID) (db.WorkerGroup, error) {
	return s.group, nil
}

func (s *adminPoolStore) ListWorkerPools(context.Context, pgtype.UUID) ([]db.WorkerPool, error) {
	return append([]db.WorkerPool(nil), s.pools...), nil
}

func (s *adminPoolStore) LockWorkerGroupForPoolMutation(context.Context, pgtype.UUID) (db.WorkerGroup, error) {
	s.transactionActions = append(s.transactionActions, "group")
	return s.group, nil
}

func (s *adminPoolStore) LockWorkerPool(context.Context, db.LockWorkerPoolParams) (db.WorkerPool, error) {
	s.transactionActions = append(s.transactionActions, "pool")
	return s.pool, nil
}

func (s *adminPoolStore) CreatePendingWorkerPool(_ context.Context, params db.CreatePendingWorkerPoolParams) (db.WorkerPool, error) {
	s.transactionActions = append(s.transactionActions, "create")
	s.createCalls++
	s.createParams = params
	created := s.created
	if !created.ID.Valid {
		created = db.WorkerPool{
			ID: params.WorkerPoolID, WorkerGroupID: params.WorkerGroupID, Name: params.Name,
			Status: "pending", ClaimVersion: 1,
		}
	}
	return created, nil
}

func (s *adminPoolStore) SetWorkerGroupPrimaryPool(_ context.Context, params db.SetWorkerGroupPrimaryPoolParams) (db.WorkerGroup, error) {
	s.transactionActions = append(s.transactionActions, "switch")
	s.switchCalls++
	s.switchParams = params
	return s.switched, nil
}

func (s *adminPoolStore) TransitionWorkerPoolLifecycle(_ context.Context, params db.TransitionWorkerPoolLifecycleParams) (db.WorkerPool, error) {
	s.transactionActions = append(s.transactionActions, "transition")
	s.transitionCalls++
	s.transitionParams = params
	if s.transitionErr != nil {
		return db.WorkerPool{}, s.transitionErr
	}
	return s.transitioned, nil
}

func TestAdminWorkerPoolListReturnsCurrentPrimaryRepresentation(t *testing.T) {
	group, pool := adminPoolFixture()
	group.PrimaryPoolID = pool.ID
	store := newAdminPoolStore(group, pool)
	response := httptest.NewRecorder()
	adminHTTPRouter(t, store).ServeHTTP(response, adminHTTPRequest(
		http.MethodGet,
		"/admin/api/v1/worker-groups/"+pgvalue.UUIDString(group.ID)+"/pools",
		"",
	))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var body api.AdminWorkerPoolsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.WorkerPools) != 1 || body.WorkerPools[0].ID != uuid.UUID(pool.ID.Bytes).String() ||
		!body.WorkerPools[0].Primary {
		t.Fatalf("Worker Pools = %+v", body.WorkerPools)
	}
}

func TestAdminWorkerPoolIDsRequireCanonicalUUIDv7(t *testing.T) {
	group, pool := adminPoolFixture()
	store := newAdminPoolStore(group, pool)
	for _, path := range []string{
		"/admin/api/v1/worker-groups/not-a-group/pools",
		"/admin/api/v1/worker-groups/" + pgvalue.UUIDString(group.ID) + "/pools/" + uuid.New().String() + "/drain",
	} {
		response := httptest.NewRecorder()
		adminHTTPRouter(t, store).ServeHTTP(response, adminHTTPRequest(http.MethodPost, path, `{}`))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400: %s", path, response.Code, response.Body.String())
		}
	}
}

func adminPoolFixture() (db.WorkerGroup, db.WorkerPool) {
	groupID := pgvalue.UUID(uuid.NewV7())
	poolID := pgvalue.UUID(uuid.NewV7())
	return db.WorkerGroup{
		ID: groupID, RegionID: "default", Name: "default", Status: db.WorkerGroupStatusActive,
		ClaimVersion: 4,
	}, db.WorkerPool{
		ID: poolID, WorkerGroupID: groupID, Name: "run-next", Status: "active",
		ClaimVersion: 4,
		SealedAt:     pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	}
}

func assertAdminPoolActions(t *testing.T, store *adminPoolStore, want ...string) {
	t.Helper()
	if len(store.transactionActions) != len(want) {
		t.Fatalf("transaction actions = %v, want %v", store.transactionActions, want)
	}
	for index := range want {
		if store.transactionActions[index] != want[index] {
			t.Fatalf("transaction actions = %v, want %v", store.transactionActions, want)
		}
	}
}

func jsonNumber(value int64) string {
	return strconv.FormatInt(value, 10)
}
