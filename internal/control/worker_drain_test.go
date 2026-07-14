package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type workerDrainStore struct {
	*fakeStore
	workerID       uuid.UUID
	credentialID   uuid.UUID
	workerGroupID  string
	epochStartedAt time.Time
	state          db.WorkerInstanceState
	completion     *db.CompleteWorkerDrainParams
	completionErr  error
	ambiguousFirst bool
	completeCalls  int
}

func newWorkerDrainStore(state db.WorkerInstanceState) *workerDrainStore {
	return &workerDrainStore{
		fakeStore:      &fakeStore{},
		workerID:       uuid.Must(uuid.NewV7()),
		credentialID:   uuid.Must(uuid.NewV7()),
		workerGroupID:  "run-workers",
		epochStartedAt: time.Now().UTC().Add(-time.Minute),
		state:          state,
	}
}

func (s *workerDrainStore) workerAuthRow() db.AuthorizeWorkerInstanceCredentialRow {
	return db.AuthorizeWorkerInstanceCredentialRow{
		ID: pgvalue.UUID(s.credentialID), WorkerGroupID: s.workerGroupID,
		WorkerInstanceID: pgvalue.UUID(s.workerID), ClaimVersion: 1,
		ProtocolVersion: auth.WorkerProtocolVersion, ResourceID: "worker-01",
		CurrentEpoch: pgtype.Int8{Int64: 7, Valid: true}, WorkerState: s.state,
		SupportsBuild: true, EpochStartedAt: pgvalue.Timestamptz(s.epochStartedAt),
	}
}

func (s *workerDrainStore) AuthorizeWorkerInstanceCredential(_ context.Context, _ db.AuthorizeWorkerInstanceCredentialParams) (db.AuthorizeWorkerInstanceCredentialRow, error) {
	if s.state == db.WorkerInstanceStateDisabled {
		return db.AuthorizeWorkerInstanceCredentialRow{}, pgx.ErrNoRows
	}
	return s.workerAuthRow(), nil
}

func (s *workerDrainStore) AuthorizeTerminalWorkerInstanceCredential(_ context.Context, _ db.AuthorizeTerminalWorkerInstanceCredentialParams) (db.AuthorizeTerminalWorkerInstanceCredentialRow, error) {
	return db.AuthorizeTerminalWorkerInstanceCredentialRow(s.workerAuthRow()), nil
}

func (s *workerDrainStore) LockWorkerDrainCompletion(_ context.Context, arg db.LockWorkerDrainCompletionParams) (pgtype.UUID, error) {
	if arg.WorkerInstanceID != pgvalue.UUID(s.workerID) || arg.WorkerGroupID != s.workerGroupID || !arg.WorkerEpoch.Valid || arg.WorkerEpoch.Int64 != 7 {
		return pgtype.UUID{}, pgx.ErrNoRows
	}
	return pgvalue.UUID(s.workerID), nil
}

func (s *workerDrainStore) CompleteWorkerDrain(_ context.Context, arg db.CompleteWorkerDrainParams) (db.CompleteWorkerDrainRow, error) {
	s.completeCalls++
	if s.completionErr != nil {
		return db.CompleteWorkerDrainRow{}, s.completionErr
	}
	if s.completion == nil {
		copy := arg
		copy.CleanupEvidence = append([]byte(nil), arg.CleanupEvidence...)
		s.completion = &copy
		s.state = db.WorkerInstanceStateDisabled
		if s.ambiguousFirst {
			return db.CompleteWorkerDrainRow{}, errors.New("connection closed after commit")
		}
	} else if s.completion.CleanupFingerprint != arg.CleanupFingerprint || string(s.completion.CleanupEvidence) != string(arg.CleanupEvidence) {
		return db.CompleteWorkerDrainRow{}, pgx.ErrNoRows
	}
	return db.CompleteWorkerDrainRow{
		ID: pgvalue.UUID(s.workerID), WorkerGroupID: s.workerGroupID,
		State:                   db.WorkerInstanceStateDisabled,
		DrainCleanupFingerprint: arg.CleanupFingerprint,
		DrainCleanupEvidence:    append([]byte(nil), arg.CleanupEvidence...),
	}, nil
}

func (s *workerDrainStore) GetWorkerInstanceState(_ context.Context, _ db.GetWorkerInstanceStateParams) (db.GetWorkerInstanceStateRow, error) {
	return db.GetWorkerInstanceStateRow{
		ID: pgvalue.UUID(s.workerID), WorkerGroupID: s.workerGroupID,
		State: s.state,
	}, nil
}

func issueDrainWorkerToken(t *testing.T, store *workerDrainStore, secret []byte) string {
	t.Helper()
	now := time.Now().UTC()
	token, err := auth.IssueWorkerToken(secret, auth.WorkerClaims{
		WorkerGroupID: store.workerGroupID, WorkerInstanceID: store.workerID.String(),
		CredentialID: store.credentialID.String(), WorkerEpoch: 7,
		ClaimVersion: 1, GroupClaimVersion: 1, Roles: []string{auth.WorkerRoleBuild},
		ProtocolVersion: auth.WorkerProtocolVersion, IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func drainCompletionBody(t *testing.T, observedAt time.Time) string {
	t.Helper()
	body, err := json.Marshal(api.WorkerDrainCompletionRequest{
		InventoryComplete: true, InventoryScope: "worker_runtime_state_roots_v0",
		ObservedAt: observedAt, Inventory: []string{}, Reclaimed: []string{},
		Quarantined: []string{}, Errors: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func serveWorkerRequest(handler http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("authorization", "Bearer "+token)
	request.Header.Set("content-type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestWorkerDrainCompletionSurvivesAmbiguousCommitAndAuthorizesOnlyTerminalRoutes(t *testing.T) {
	secret := []byte("01234567890123456789012345678901")
	store := newWorkerDrainStore(db.WorkerInstanceStateDraining)
	store.ambiguousFirst = true
	handler := newTestServer(testServerConfig{DB: store, WorkerTokenSecret: secret})
	token := issueDrainWorkerToken(t, store, secret)
	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	body := drainCompletionBody(t, observedAt)

	first := serveWorkerRequest(handler, token, http.MethodPost, "/api/worker/drain/complete", body)
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first completion status = %d, body = %s", first.Code, first.Body.String())
	}
	second := serveWorkerRequest(handler, token, http.MethodPost, "/api/worker/drain/complete", body)
	if second.Code != http.StatusOK {
		t.Fatalf("retry completion status = %d, body = %s", second.Code, second.Body.String())
	}
	if store.completeCalls != 2 || store.completion == nil || len(store.completion.CleanupFingerprint.String) != 64 {
		t.Fatalf("completion calls=%d proof=%+v", store.completeCalls, store.completion)
	}
	var evidence canonicalWorkerDrainCleanupEvidence
	if err := json.Unmarshal(store.completion.CleanupEvidence, &evidence); err != nil {
		t.Fatal(err)
	}
	if !evidence.InventoryComplete || evidence.InventoryScope != "worker_runtime_state_roots_v0" || evidence.Inventory == nil || evidence.Reclaimed == nil || evidence.Quarantined == nil || evidence.Errors == nil {
		t.Fatalf("canonical evidence = %+v", evidence)
	}

	status := serveWorkerRequest(handler, token, http.MethodGet, "/api/worker/status", "")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"status":"disabled"`) {
		t.Fatalf("disabled status = %d, body = %s", status.Code, status.Body.String())
	}
	observe := serveWorkerRequest(handler, token, http.MethodPost, "/api/worker/observe", `{"observation":{}}`)
	if observe.Code != http.StatusUnauthorized {
		t.Fatalf("disabled mutation status = %d, body = %s", observe.Code, observe.Body.String())
	}
	different := serveWorkerRequest(handler, token, http.MethodPost, "/api/worker/drain/complete", drainCompletionBody(t, observedAt.Add(time.Microsecond)))
	if different.Code != http.StatusConflict {
		t.Fatalf("different proof replay status = %d, body = %s", different.Code, different.Body.String())
	}
}

func TestWorkerDrainCompletionRejectsIncompleteAuthority(t *testing.T) {
	secret := []byte("01234567890123456789012345678901")
	store := newWorkerDrainStore(db.WorkerInstanceStateDraining)
	store.completionErr = pgx.ErrNoRows
	handler := newTestServer(testServerConfig{DB: store, WorkerTokenSecret: secret})
	response := serveWorkerRequest(handler, issueDrainWorkerToken(t, store, secret), http.MethodPost,
		"/api/worker/drain/complete", drainCompletionBody(t, time.Now().UTC()))
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestWorkerDrainCompletionValidatesPhysicalCleanupProof(t *testing.T) {
	secret := []byte("01234567890123456789012345678901")
	store := newWorkerDrainStore(db.WorkerInstanceStateDraining)
	handler := newTestServer(testServerConfig{DB: store, WorkerTokenSecret: secret})
	token := issueDrainWorkerToken(t, store, secret)
	now := time.Now().UTC()
	tests := []struct {
		name string
		body api.WorkerDrainCompletionRequest
	}{
		{name: "incomplete", body: api.WorkerDrainCompletionRequest{InventoryScope: "worker_runtime_state_roots_v0", ObservedAt: now}},
		{name: "wrong scope", body: api.WorkerDrainCompletionRequest{InventoryComplete: true, InventoryScope: "other", ObservedAt: now}},
		{name: "zero timestamp", body: api.WorkerDrainCompletionRequest{InventoryComplete: true, InventoryScope: "worker_runtime_state_roots_v0"}},
		{name: "future timestamp", body: api.WorkerDrainCompletionRequest{InventoryComplete: true, InventoryScope: "worker_runtime_state_roots_v0", ObservedAt: now.Add(2 * time.Minute)}},
		{name: "previous epoch", body: api.WorkerDrainCompletionRequest{InventoryComplete: true, InventoryScope: "worker_runtime_state_roots_v0", ObservedAt: store.epochStartedAt.Add(-time.Nanosecond)}},
		{name: "inventory", body: api.WorkerDrainCompletionRequest{InventoryComplete: true, InventoryScope: "worker_runtime_state_roots_v0", ObservedAt: now, Inventory: []string{"runtime"}}},
		{name: "reclaimed", body: api.WorkerDrainCompletionRequest{InventoryComplete: true, InventoryScope: "worker_runtime_state_roots_v0", ObservedAt: now, Reclaimed: []string{"runtime"}}},
		{name: "quarantined", body: api.WorkerDrainCompletionRequest{InventoryComplete: true, InventoryScope: "worker_runtime_state_roots_v0", ObservedAt: now, Quarantined: []string{"runtime"}}},
		{name: "errors", body: api.WorkerDrainCompletionRequest{InventoryComplete: true, InventoryScope: "worker_runtime_state_roots_v0", ObservedAt: now, Errors: []string{"cleanup failed"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(test.body)
			if err != nil {
				t.Fatal(err)
			}
			response := serveWorkerRequest(handler, token, http.MethodPost, "/api/worker/drain/complete", string(body))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
	if store.completeCalls != 0 {
		t.Fatalf("complete calls = %d, want 0", store.completeCalls)
	}
}

func TestCompleteWorkerDrainAtomicallyFencesCurrentEpochAuthority(t *testing.T) {
	ctx := context.Background()
	pool := newControlIntegrationDB(t, ctx)
	ids := seedControlStreamTokenFixture(t, ctx, pool)
	queries := db.New(pool)
	server := &Server{db: queries, tx: pool}
	workerID := uuid.Must(uuid.NewV7())
	serviceID := uuid.Must(uuid.NewV7())
	epochStartedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (
			id, worker_group_id, resource_id, attestation_fingerprint, state,
			current_epoch, current_service_id, protocol_version, supports_build,
			certified_cpu_millis, certified_memory_bytes, certified_workload_disk_bytes,
			certified_scratch_bytes, per_vm_cpu_millis, per_vm_memory_bytes,
			per_vm_workload_disk_bytes, per_vm_scratch_bytes, max_build_executors,
			certification_profile, certification_fingerprint, epoch_started_at,
			certified_at, activated_at, draining_at
		) VALUES (
			$1, $2, $3, 'sha256:test-attestation', 'draining',
			2, $4, $5, true,
			1000, 1073741824, 4294967296,
			1073741824, 1000, 1073741824,
			4294967296, 1073741824, 1,
			'drain-test', 'drain-test-cert', $6, $6, $6, $6
		)
	`, workerID, dbtest.DefaultWorkerGroupID, "worker-"+workerID.String()[:8], serviceID, auth.WorkerProtocolVersion, epochStartedAt); err != nil {
		t.Fatal(err)
	}
	var sandboxID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT id FROM deployment_sandboxes
		 WHERE org_id = $1 AND project_id = $2 AND environment_id = $3
		 ORDER BY id LIMIT 1
	`, ids.orgID, ids.projectID, ids.environmentID).Scan(&sandboxID); err != nil {
		t.Fatal(err)
	}
	insertRuntime := func(epoch int64) uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		if _, err := pool.Exec(ctx, `
			INSERT INTO runtime_instances (
				id, org_id, worker_group_id, project_id, environment_id, region_id,
				worker_instance_id, runtime_identity_id, deployment_sandbox_id, worker_epoch,
				runtime_key_hash, runtime_key, sandbox_fingerprint, rootfs_digest,
				image_digest, image_format, runtime_abi, guestd_abi, adapter_abi,
				network_policy, reserved_cpu_millis, reserved_memory_bytes,
				reserved_workload_disk_bytes, reserved_scratch_bytes,
				reserved_execution_slots, desired_reason
			) VALUES (
				$1, $2, $3, $4, $5, $6,
				$7, 'test-runtime', $8, $9,
				$10, '{}', 'sandbox-fingerprint', 'sha256:rootfs',
				'sha256:image', 'oci-tar', 'test', 'guestd-test', 'adapter-test',
				'{}', 1000, 1073741824, 1073741824, 0, 1, 'drain-test'
			)
		`, id, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID,
			dbtest.DefaultRegionID, workerID, sandboxID, epoch, "runtime-"+id.String()); err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldEpochRuntimeID := insertRuntime(1)
	currentRuntimeID := insertRuntime(2)
	evidence := []byte(`{"inventory_complete":true,"inventory_scope":"worker_runtime_state_roots_v0","observed_at":"2026-07-14T00:00:00Z","inventory":[],"reclaimed":[],"quarantined":[],"errors":[]}`)
	params := db.CompleteWorkerDrainParams{
		WorkerInstanceID: pgvalue.UUID(workerID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
		WorkerEpoch:        pgtype.Int8{Int64: 2, Valid: true},
		CleanupFingerprint: pgtype.Text{String: strings.Repeat("a", 64), Valid: true},
		CleanupEvidence:    evidence, ObservedAt: pgvalue.Timestamptz(time.Now().UTC()),
	}
	if _, err := server.completeWorkerDrain(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("completion with current authority error = %v, want no rows", err)
	}
	var state db.WorkerInstanceState
	if err := pool.QueryRow(ctx, `SELECT state FROM worker_instances WHERE id = $1`, workerID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.WorkerInstanceStateDraining {
		t.Fatalf("state after rejected completion = %q", state)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET desired_state = 'closed', desired_version = 2,
		       observed_state = 'closed', observed_version = 1, observed_desired_version = 2,
		       closing_at = now(), closed_at = now(), terminal_at = now(),
		       terminal_reason_code = 'drain_test', reclaimed_at = now()
		 WHERE id = $1
	`, currentRuntimeID); err != nil {
		t.Fatal(err)
	}

	// A grant transaction can acquire the worker fence before drain completion
	// and commit new authority while the completion transaction waits. The
	// completion must take a fresh statement snapshot after acquiring that lock;
	// otherwise one large lock-and-update statement can miss the new runtime.
	grantTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	grantCommitted := false
	defer func() {
		if !grantCommitted {
			_ = grantTx.Rollback(context.Background())
		}
	}()
	if _, err := grantTx.Exec(ctx, `SELECT id FROM worker_instances WHERE id = $1 FOR UPDATE`, workerID); err != nil {
		t.Fatal(err)
	}
	racedRuntimeID := uuid.Must(uuid.NewV7())
	if _, err := grantTx.Exec(ctx, `
		INSERT INTO runtime_instances (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, runtime_identity_id, deployment_sandbox_id, worker_epoch,
			runtime_key_hash, runtime_key, sandbox_fingerprint, rootfs_digest,
			image_digest, image_format, runtime_abi, guestd_abi, adapter_abi,
			network_policy, reserved_cpu_millis, reserved_memory_bytes,
			reserved_workload_disk_bytes, reserved_scratch_bytes,
			reserved_execution_slots, desired_reason
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, 'test-runtime', $8, 2,
			$9, '{}', 'sandbox-fingerprint', 'sha256:rootfs',
			'sha256:image', 'oci-tar', 'test', 'guestd-test', 'adapter-test',
			'{}', 1000, 1073741824, 1073741824, 0, 1, 'drain-race-test'
		)
	`, racedRuntimeID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID,
		ids.environmentID, dbtest.DefaultRegionID, workerID, sandboxID,
		"runtime-"+racedRuntimeID.String()); err != nil {
		t.Fatal(err)
	}
	drainTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = drainTx.Rollback(context.Background()) }()
	drainQueries := db.New(drainTx)
	drainResult := make(chan error, 1)
	go func() {
		_, lockErr := drainQueries.LockWorkerDrainCompletion(ctx, db.LockWorkerDrainCompletionParams{
			WorkerInstanceID: params.WorkerInstanceID,
			WorkerGroupID:    params.WorkerGroupID,
			WorkerEpoch:      params.WorkerEpoch,
		})
		if lockErr != nil {
			drainResult <- lockErr
			return
		}
		_, completeErr := drainQueries.CompleteWorkerDrain(ctx, params)
		drainResult <- completeErr
	}()
	select {
	case err := <-drainResult:
		t.Fatalf("drain completed before worker-fenced grant committed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := grantTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	grantCommitted = true
	if err := <-drainResult; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("completion racing with committed authority error = %v, want no rows", err)
	}
	if err := drainTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET desired_state = 'closed', desired_version = 2,
		       observed_state = 'closed', observed_version = 1, observed_desired_version = 2,
		       closing_at = now(), closed_at = now(), terminal_at = now(),
		       terminal_reason_code = 'drain_race_test', reclaimed_at = now()
		 WHERE id = $1
	`, racedRuntimeID); err != nil {
		t.Fatal(err)
	}
	completed, err := server.completeWorkerDrain(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != db.WorkerInstanceStateDisabled {
		t.Fatalf("completed state = %q", completed.State)
	}
	if _, err := server.completeWorkerDrain(ctx, params); err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	different := params
	different.CleanupFingerprint = pgtype.Text{String: strings.Repeat("b", 64), Valid: true}
	different.CleanupEvidence = []byte(`{"different":true}`)
	if _, err := server.completeWorkerDrain(ctx, different); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("different replay error = %v, want no rows", err)
	}
	var claimVersion int64
	var storedFingerprint pgtype.Text
	var storedEvidence []byte
	if err := pool.QueryRow(ctx, `
		SELECT state, claim_version, drain_cleanup_fingerprint, drain_cleanup_evidence
		  FROM worker_instances WHERE id = $1
	`, workerID).Scan(&state, &claimVersion, &storedFingerprint, &storedEvidence); err != nil {
		t.Fatal(err)
	}
	var storedJSON any
	var expectedJSON any
	if err := json.Unmarshal(storedEvidence, &storedJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(evidence, &expectedJSON); err != nil {
		t.Fatal(err)
	}
	if state != db.WorkerInstanceStateDisabled || claimVersion != 1 || storedFingerprint.String != params.CleanupFingerprint.String || !reflect.DeepEqual(storedJSON, expectedJSON) {
		t.Fatalf("stored completion state=%q claim=%d fingerprint=%q evidence=%s", state, claimVersion, storedFingerprint.String, storedEvidence)
	}
	var oldReclaimed pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT reclaimed_at FROM runtime_instances WHERE id = $1`, oldEpochRuntimeID).Scan(&oldReclaimed); err != nil {
		t.Fatal(err)
	}
	if oldReclaimed.Valid {
		t.Fatal("old-epoch authority was mutated by current-epoch drain completion")
	}
}
