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
	"github.com/jackc/pgx/v5"
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

func (s *adminPoolStore) BeginQuerier(context.Context) (db.Querier, transaction, error) {
	return s, &adminHTTPTransaction{}, nil
}

func (s *adminPoolStore) GetWorkerGroup(context.Context, string) (db.WorkerGroup, error) {
	return s.group, nil
}

func (s *adminPoolStore) ListWorkerPools(context.Context, string) ([]db.WorkerPool, error) {
	return append([]db.WorkerPool(nil), s.pools...), nil
}

func (s *adminPoolStore) LockWorkerGroupForPoolMutation(context.Context, string) (db.WorkerGroup, error) {
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
			State: "pending", ClaimVersion: 1,
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
		"/admin/api/v1/worker-groups/"+group.ID+"/pools",
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

func TestAdminWorkerPoolCreateLocksGroupAndCreatesPendingPool(t *testing.T) {
	group, pool := adminPoolFixture()
	store := newAdminPoolStore(group, pool)
	response := httptest.NewRecorder()
	adminHTTPRouter(t, store).ServeHTTP(response, adminHTTPRequest(
		http.MethodPost,
		"/admin/api/v1/worker-groups/"+group.ID+"/pools",
		`{"name":"run-next","expected_group_claim_version":4}`,
	))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", response.Code, response.Body.String())
	}
	if store.createCalls != 1 || store.createParams.WorkerGroupID != group.ID || store.createParams.Name != "run-next" ||
		store.createParams.ExpectedGroupClaimVersion != 4 {
		t.Fatalf("create params = %+v, calls = %d", store.createParams, store.createCalls)
	}
	if _, err := pgvalue.UUIDValue(store.createParams.WorkerPoolID); err != nil {
		t.Fatalf("generated Worker Pool ID: %v", err)
	}
	assertAdminPoolActions(t, store, "group", "create")
	var body api.AdminWorkerPool
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Name != "run-next" || body.State != "pending" || body.ClaimVersion != 1 {
		t.Fatalf("created Worker Pool = %+v", body)
	}
}

func TestAdminWorkerPoolPrimarySwitchIsFencedAndReplaySafe(t *testing.T) {
	t.Run("apply", func(t *testing.T) {
		group, pool := adminPoolFixture()
		store := newAdminPoolStore(group, pool)
		store.switched = group
		store.switched.ClaimVersion++
		store.switched.PrimaryPoolID = pool.ID

		response := httptest.NewRecorder()
		adminHTTPRouter(t, store).ServeHTTP(response, adminHTTPRequest(
			http.MethodPost,
			"/admin/api/v1/worker-groups/"+group.ID+"/pools/"+uuid.UUID(pool.ID.Bytes).String()+"/primary",
			`{"expected_group_claim_version":4}`,
		))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
		}
		if store.switchCalls != 1 || store.switchParams.PoolID != pool.ID ||
			store.switchParams.ExpectedGroupClaimVersion != 4 {
			t.Fatalf("switch params = %+v, calls = %d", store.switchParams, store.switchCalls)
		}
		assertAdminPoolActions(t, store, "group", "pool", "switch")
		var body api.SwitchAdminWorkerPoolPrimaryResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.WorkerGroup.ClaimVersion != 5 || body.WorkerGroup.PrimaryPoolID != uuid.UUID(pool.ID.Bytes).String() ||
			!body.WorkerPool.Primary {
			t.Fatalf("primary switch response = %+v", body)
		}
	})

	t.Run("exact replay", func(t *testing.T) {
		group, pool := adminPoolFixture()
		group.ClaimVersion = 5
		group.PrimaryPoolID = pool.ID
		store := newAdminPoolStore(group, pool)

		response := httptest.NewRecorder()
		adminHTTPRouter(t, store).ServeHTTP(response, adminHTTPRequest(
			http.MethodPost,
			"/admin/api/v1/worker-groups/"+group.ID+"/pools/"+uuid.UUID(pool.ID.Bytes).String()+"/primary",
			`{"expected_group_claim_version":4}`,
		))
		if response.Code != http.StatusOK || store.switchCalls != 0 {
			t.Fatalf("status = %d, switch calls = %d: %s", response.Code, store.switchCalls, response.Body.String())
		}
		assertAdminPoolActions(t, store, "group", "pool")
	})
}

func TestAdminWorkerPoolLifecycleLocksGroupBeforePool(t *testing.T) {
	for _, test := range []struct {
		name       string
		path       string
		state      string
		target     string
		claim      int64
		transition int64
	}{
		{name: "drain active", path: "drain", state: "active", target: "draining", claim: 4, transition: 5},
		{name: "disable pending", path: "disable", state: "pending", target: "disabled", claim: 1, transition: 2},
		{name: "disable drained", path: "disable", state: "draining", target: "disabled", claim: 5, transition: 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			group, pool := adminPoolFixture()
			pool.State = test.state
			pool.ClaimVersion = test.claim
			store := newAdminPoolStore(group, pool)
			store.transitioned = pool
			store.transitioned.State = test.target
			store.transitioned.ClaimVersion = test.transition

			response := httptest.NewRecorder()
			adminHTTPRouter(t, store).ServeHTTP(response, adminHTTPRequest(
				http.MethodPost,
				"/admin/api/v1/worker-groups/"+group.ID+"/pools/"+uuid.UUID(pool.ID.Bytes).String()+"/"+test.path,
				`{"expected_pool_claim_version":`+jsonNumber(test.claim)+`}`,
			))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
			}
			if store.transitionCalls != 1 || store.transitionParams.TargetState != test.target ||
				store.transitionParams.ExpectedPoolClaimVersion != test.claim {
				t.Fatalf("transition params = %+v, calls = %d", store.transitionParams, store.transitionCalls)
			}
			assertAdminPoolActions(t, store, "group", "pool", "transition")
			var body api.AdminWorkerPool
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.State != test.target || body.ClaimVersion != test.transition {
				t.Fatalf("transition response = %+v", body)
			}
		})
	}
}

func TestAdminWorkerPoolLifecyclePreservesRestorableSupplySafetyFence(t *testing.T) {
	group, pool := adminPoolFixture()
	store := newAdminPoolStore(group, pool)
	store.transitionErr = pgx.ErrNoRows
	response := httptest.NewRecorder()
	adminHTTPRouter(t, store).ServeHTTP(response, adminHTTPRequest(
		http.MethodPost,
		"/admin/api/v1/worker-groups/"+group.ID+"/pools/"+uuid.UUID(pool.ID.Bytes).String()+"/drain",
		`{"expected_pool_claim_version":4}`,
	))
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if got := decodeHTTPError(t, response.Body.Bytes()).Code; got != "conflict" {
		t.Fatalf("error code = %q, want conflict", got)
	}
	assertAdminPoolActions(t, store, "group", "pool", "transition")
}

func TestAdminWorkerPoolIDsRequireCanonicalUUIDv7(t *testing.T) {
	group, pool := adminPoolFixture()
	store := newAdminPoolStore(group, pool)
	for _, path := range []string{
		"/admin/api/v1/worker-groups/not-a-group/pools",
		"/admin/api/v1/worker-groups/" + group.ID + "/pools/" + uuid.New().String() + "/drain",
	} {
		response := httptest.NewRecorder()
		adminHTTPRouter(t, store).ServeHTTP(response, adminHTTPRequest(http.MethodPost, path, `{}`))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400: %s", path, response.Code, response.Body.String())
		}
	}
}

func adminPoolFixture() (db.WorkerGroup, db.WorkerPool) {
	groupID := uuid.NewV7().String()
	poolID := pgvalue.UUID(uuid.NewV7())
	return db.WorkerGroup{
		ID: groupID, RegionID: "default", Name: "default", State: db.WorkerGroupStateActive,
		ClaimVersion: 4,
	}, db.WorkerPool{
		ID: poolID, WorkerGroupID: groupID, Name: "run-next", State: "active",
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
