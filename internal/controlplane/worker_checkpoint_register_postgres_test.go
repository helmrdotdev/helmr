package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const (
	checkpointRegisterPath = "/worker/v1/computer/checkpoints/register"
	checkpointReadyPath    = "/worker/v1/computer/checkpoints/ready"
	checkpointAbortPath    = "/worker/v1/computer/checkpoints/abort"
)

// computerCheckpointFixture serves a registered capture's worker host
// through NewServer, with object storage the host uploads to.
type computerCheckpointFixture struct {
	runtest.Fixture
	queries *db.Queries
	store   testUploadStore
	worker  workerHTTPClient
}

func checkpointRegistrationFixture(t *testing.T) (*computerCheckpointFixture, workerapi.RegisterCheckpointRequest) {
	t.Helper()
	base, ref, manifest := computertest.RegisteredCapture(t, false)
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE runs SET active_started_at=clock_timestamp(),max_active_duration_ms=3600000 WHERE current_run_lease_id IN (SELECT id FROM run_leases WHERE computer_instance_id=$1)`, ref.InstanceID)
	store := newTestUploadStore(t)
	key, err := disk.NewFencingKey(make([]byte, disk.FencingKeySize))
	if err != nil {
		t.Fatal(err)
	}
	hash, err := computer.WriterTokenHash(key, ref.InstanceID, uuid.MustParse(manifest.RecoveryPoint.ComputerID), manifest.RecoveryPoint.WriterGeneration)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE computer_instances SET writer_token_hash=$2 WHERE id=$1`, ref.InstanceID, hash)
	handler := newPostgresServer(t, base.Pool, func(cfg *ServerConfig) { cfg.CAS = store; cfg.ComputerFencingKey = key })
	return &computerCheckpointFixture{Fixture: base, queries: db.New(base.Pool), store: store, worker: newWorkerHTTPClient(t, handler, base.Pool, base.WorkerID)},
		workerRegisterCheckpointRequest(t, ref, manifest)
}

// workerRegisterCheckpointRequest is the worker host's registration of the
// candidate: the owner manifest has the wire manifest's encoding.
func workerRegisterCheckpointRequest(t *testing.T, ref computer.CheckpointRef, manifest computer.CheckpointManifest) workerapi.RegisterCheckpointRequest {
	t.Helper()
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	request := workerapi.RegisterCheckpointRequest{ComputerInstanceID: ref.InstanceID.String(), WorkerEpoch: ref.WorkerEpoch, DesiredVersion: ref.DesiredVersion, CheckpointID: ref.CheckpointID.String()}
	if err = json.Unmarshal(encoded, &request.Manifest); err != nil {
		t.Fatal(err)
	}
	return request
}

func TestCheckpointRegistrationPinsCompleteCandidateUntilInvalidation(t *testing.T) {
	f, req := checkpointRegistrationFixture(t)
	var response workerapi.ComputerCheckpointResponse
	f.worker.post(t, checkpointRegisterPath, req, http.StatusOK, &response)
	if response.CheckpointID != req.CheckpointID || response.ComputerDiskVersionID != "" {
		t.Fatalf("registration published a version: %+v", response)
	}
	// Nothing was uploaded. All four runtime descriptors are owned first.
	rows, err := f.queries.ListCheckpointObjects(t.Context(), pgvalue.UUID(uuid.MustParse(req.CheckpointID)))
	if err != nil || len(rows) != 4 {
		t.Fatalf("objects=%d %v", len(rows), err)
	}
	for _, row := range rows {
		var observed int
		if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM cas_objects WHERE digest=$1`, row.Digest).Scan(&observed); err != nil || observed != 0 {
			t.Fatalf("registration observed storage: %d %v", observed, err)
		}
		if _, err := f.Pool.Exec(t.Context(), `UPDATE cas_blobs SET retired_at=now(),next_reclaim_at=now() WHERE digest=$1`, row.Digest); err == nil {
			t.Fatal("creating candidate lost pin")
		}
	}
	f.worker.post(t, checkpointRegisterPath, req, http.StatusOK, nil)
	changed := req
	changed.Manifest.RuntimeState.Config = json.RawMessage(`{"different":true}`)
	f.worker.post(t, checkpointRegisterPath, changed, http.StatusConflict, nil)
	// The abort receipt releases unpublished objects while retaining the source.
	f.worker.post(t, checkpointAbortPath, workerapi.CaptureAbortRequest{ComputerInstanceID: req.ComputerInstanceID, WorkerEpoch: req.WorkerEpoch, DesiredVersion: req.DesiredVersion, CheckpointID: req.CheckpointID}, http.StatusOK, nil)
	collectible, err := f.queries.ListAbandonedCasBlobs(t.Context(), 100)
	if err != nil || len(collectible) != 4 {
		t.Fatalf("collector cannot discover failed set: %v %v", collectible, err)
	}
	for _, row := range rows {
		if n, err := f.queries.RetireAbandonedCasBlob(t.Context(), row.Digest); err != nil || n != 1 {
			t.Fatalf("failed checkpoint object not collectible: %d %v", n, err)
		}
	}
	f.worker.post(t, checkpointRegisterPath, req, http.StatusConflict, nil)
}

func TestCheckpointRegistrationRollsBackWholeSetOnRetiredMember(t *testing.T) {
	f, req := checkpointRegistrationFixture(t)
	digest := req.Manifest.RuntimeState.MemoryArtifacts[0].Digest
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_blobs(digest,size_bytes,retired_at,next_reclaim_at) VALUES($1,$2,now(),now())`, digest, req.Manifest.RuntimeState.MemoryArtifacts[0].SizeBytes)
	if response := f.worker.send(t, checkpointRegisterPath, req); response.Code == http.StatusOK {
		t.Fatal("adopted retired memory")
	}
	rows, err := f.queries.ListCheckpointObjects(t.Context(), pgvalue.UUID(uuid.MustParse(req.CheckpointID)))
	if err != nil || len(rows) != 0 {
		t.Fatalf("partial registration survived: %d %v", len(rows), err)
	}
	var empty bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT manifest IS NULL FROM computer_checkpoints WHERE id=$1`, req.CheckpointID).Scan(&empty); err != nil || !empty {
		t.Fatalf("partial manifest survived: %v %v", empty, err)
	}
}

func TestCheckpointRegistrationRejectsWrongSource(t *testing.T) {
	f, req := checkpointRegistrationFixture(t)
	for _, change := range []struct {
		status int
		apply  func(*workerapi.RegisterCheckpointRequest)
	}{
		{http.StatusConflict, func(r *workerapi.RegisterCheckpointRequest) { r.DesiredVersion++ }},
		{http.StatusBadRequest, func(r *workerapi.RegisterCheckpointRequest) {
			r.Manifest.RecoveryPoint.ComputerInstanceID = uuid.NewV7().String()
		}},
		{http.StatusBadRequest, func(r *workerapi.RegisterCheckpointRequest) {
			copy := *r.Manifest.RuntimeState.Computer
			copy.LogicalBytes /= 2
			r.Manifest.RuntimeState.Computer = &copy
		}},
		{http.StatusBadRequest, func(r *workerapi.RegisterCheckpointRequest) {
			copy := *r.Manifest.RuntimeState.Computer
			copy.ComputerID = uuid.NewV7().String()
			r.Manifest.RuntimeState.Computer = &copy
		}},
		{http.StatusBadRequest, func(r *workerapi.RegisterCheckpointRequest) { r.CheckpointID = "not-a-uuid" }},
		{http.StatusBadRequest, func(r *workerapi.RegisterCheckpointRequest) { r.WorkerEpoch = 0 }},
	} {
		changed := req
		change.apply(&changed)
		f.worker.post(t, checkpointRegisterPath, changed, change.status, nil)
	}
	rows, err := f.queries.ListCheckpointObjects(t.Context(), pgvalue.UUID(uuid.MustParse(req.CheckpointID)))
	if err != nil || len(rows) != 0 {
		t.Fatal("stale registration left objects")
	}
}

func TestCheckpointRegistrationConcurrentIdentity(t *testing.T) {
	for _, different := range []bool{false, true} {
		t.Run(fmt.Sprint(different), func(t *testing.T) {
			f, req := checkpointRegistrationFixture(t)
			other := req
			if different {
				other.Manifest.RuntimeState.Config = json.RawMessage(`{"different":true}`)
			}
			start := make(chan struct{})
			results := make(chan int, 2)
			for _, r := range []workerapi.RegisterCheckpointRequest{req, other} {
				go func() { <-start; results <- f.worker.send(t, checkpointRegisterPath, r).Code }()
			}
			close(start)
			failures := 0
			for range 2 {
				if status := <-results; status != http.StatusOK {
					failures++
				}
			}
			want := 0
			if different {
				want = 1
			}
			if failures != want {
				t.Fatalf("conflicts=%d want %d", failures, want)
			}
			rows, err := f.queries.ListCheckpointObjects(t.Context(), pgvalue.UUID(uuid.MustParse(req.CheckpointID)))
			if err != nil || len(rows) != 4 {
				t.Fatalf("partial set: %d %v", len(rows), err)
			}
		})
	}
}

func TestCheckpointRegistrationExpiresDuringObjectLock(t *testing.T) {
	f, req := checkpointRegistrationFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	digest := req.Manifest.RuntimeState.ConfigArtifact.Digest
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,$2)`, digest, req.Manifest.RuntimeState.ConfigArtifact.SizeBytes)
	locker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(context.Background())
	dbtest.MustExec(t, ctx, locker, `SELECT digest FROM cas_blobs WHERE digest=$1 FOR UPDATE`, digest)
	var expiry time.Time
	if err := f.Pool.QueryRow(ctx, `UPDATE computer_checkpoints SET expires_at=clock_timestamp()+interval '3 seconds' WHERE id=$1 RETURNING expires_at`, req.CheckpointID).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	result := make(chan int, 1)
	go func() { result <- f.worker.send(t, checkpointRegisterPath, req).Code }()
	for {
		var blocked bool
		if err := f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, locker.Conn().PgConn().PID()).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case status := <-result:
			t.Fatalf("registration did not reach object lock: %d", status)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-time.After(time.Until(expiry) + 50*time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := locker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if status := <-result; status != http.StatusConflict {
		t.Fatalf("expired registration: %d", status)
	}
	rows, err := f.queries.ListCheckpointObjects(ctx, pgvalue.UUID(uuid.MustParse(req.CheckpointID)))
	if err != nil || len(rows) != 0 {
		t.Fatal("expired transaction retained a partial candidate")
	}
}

func TestCheckpointRegistrationCannotBypassPairedPublication(t *testing.T) {
	f, registered := checkpointRegistrationFixture(t)
	f.worker.post(t, checkpointRegisterPath, registered, http.StatusOK, nil)
	ready := workerapi.CheckpointReadyRequest(registered)
	ready.Manifest.RuntimeState.Computer = nil
	f.worker.post(t, checkpointReadyPath, ready, http.StatusBadRequest, nil)
	rows, err := f.queries.ListCheckpointObjects(t.Context(), pgvalue.UUID(uuid.MustParse(registered.CheckpointID)))
	if err != nil || len(rows) != 4 {
		t.Fatal("publication rejection lost candidate")
	}
	for _, row := range rows {
		if row.CheckpointStatus != "creating" || !row.AvailabilityRequired.Valid || !row.AvailabilityRequired.Bool {
			t.Fatalf("publication rejection released pin: %+v", row)
		}
	}
}
