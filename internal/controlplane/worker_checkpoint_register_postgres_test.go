package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func checkpointRegistrationFixture(t *testing.T) (*actorCheckpointFixture, workerapi.RegisterCheckpointRequest) {
	t.Helper()
	f := newActorCheckpointFixtureWithInput(t, nil)
	waitID := uuid.NewV7()
	seq := int64(0)
	params, _ := json.Marshal(workerActorInputWaitParams{SessionID: f.sessionID.String(), AfterInputSequence: seq})
	f.workerCall(t, f.server.workerCreateRunWait, workerapi.CreateRunWaitRequest{CorrelationID: uuid.NewV7().String(), Lease: f.fence(), RunWaitID: waitID.String(), ResumeAttachID: uuid.NewV7().String(), Kind: "actor_input", Params: params, ActorSpeculativeInputSequence: &seq}, nil)
	lease, err := parseRunLeaseFence(f.fence())
	if err != nil {
		t.Fatal(err)
	}
	wait, err := f.server.requestWorkerRunWaitCheckpoint(t.Context(), f.worker, f.fence(), lease, waitID)
	if err != nil {
		t.Fatal(err)
	}
	m := validCheckpointReadyRequest().Manifest
	m.RecoveryPoint.ID = pgvalue.UUIDString(wait.SuspendCheckpointID)
	m.RecoveryPoint.RunID = f.runID.String()
	m.RecoveryPoint.RunWaitID = waitID.String()
	rt := &m.RecoveryPoint.Runtime
	rt.ID = f.RuntimeIdentityID
	rt.KernelDigest = dbtest.Digest("run-lease-kernel")
	rt.InitramfsDigest = dbtest.Digest("run-lease-initramfs")
	rt.RootfsDigest = dbtest.Digest("run-lease-rootfs")
	rt.VMVCPUCount = 1
	rt.CPUConfigDigest = f.CPUConfigDigest
	rt.Substrate = &workerapi.CheckpointRuntimeSubstrate{Digest: dbtest.Digest("frontier-substrate"), Format: "squashfs", Contract: "builder-v0", SizeBytes: 1}
	m.RuntimeState.Computer = &workerapi.CheckpointComputer{ComputerID: f.workspaceID.String(), LogicalBytes: f.claim.runtime.ReservedGuestEphemeralDiskBytes, Root: testGenerationRoot(f.claim.runtime.ReservedGuestEphemeralDiskBytes)}
	return f, workerapi.RegisterCheckpointRequest{Lease: f.fence(), RequestVersion: wait.CheckpointRequestVersion, RunWaitID: waitID.String(), CheckpointID: m.RecoveryPoint.ID, Manifest: m}
}

func TestCheckpointRegistrationPinsCompleteCandidateUntilInvalidation(t *testing.T) {
	f, req := checkpointRegistrationFixture(t)
	var response workerapi.CheckpointResponse
	f.workerCall(t, f.server.workerRegisterCheckpoint, req, &response)
	if response.CheckpointID != req.CheckpointID || response.WorkspaceVersionID != "" {
		t.Fatalf("registration published a version: %+v", response)
	}
	// Nothing was uploaded. All four runtime descriptors are owned first.
	rows, err := f.server.db.ListCheckpointObjects(t.Context(), pgvalue.UUID(uuid.MustParse(req.CheckpointID)))
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
	f.workerCall(t, f.server.workerRegisterCheckpoint, req, nil)
	changed := req
	changed.Manifest.RuntimeState.Config = json.RawMessage(`{"different":true}`)
	if _, err := f.server.registerCheckpoint(t.Context(), f.worker, changed); err == nil {
		t.Fatal("replaced immutable candidate")
	}
	// Use the actual failed receipt path, not a test-only unpin operation.
	f.workerCall(t, f.server.workerMarkCheckpointFailed, workerapi.CheckpointFailedRequest{Lease: req.Lease, RequestVersion: req.RequestVersion, RunWaitID: req.RunWaitID, CheckpointID: req.CheckpointID, Error: "upload failed"}, nil)
	collectible, err := f.server.db.ListAbandonedCasBlobs(t.Context(), 100)
	if err != nil || len(collectible) != 4 {
		t.Fatalf("collector cannot discover failed set: %v %v", collectible, err)
	}
	for _, row := range rows {
		if n, err := f.server.db.RetireAbandonedCasBlob(t.Context(), row.Digest); err != nil || n != 1 {
			t.Fatalf("failed checkpoint object not collectible: %d %v", n, err)
		}
	}
	if _, err := f.server.registerCheckpoint(t.Context(), f.worker, req); err == nil {
		t.Fatal("reopened invalid candidate")
	}
}

func TestCheckpointRegistrationRollsBackWholeSetOnRetiredMember(t *testing.T) {
	f, req := checkpointRegistrationFixture(t)
	digest := req.Manifest.RuntimeState.MemoryArtifacts[0].Digest
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_blobs(digest,size_bytes,retired_at,next_reclaim_at) VALUES($1,$2,now(),now())`, digest, req.Manifest.RuntimeState.MemoryArtifacts[0].SizeBytes)
	if _, err := f.server.registerCheckpoint(t.Context(), f.worker, req); err == nil {
		t.Fatal("adopted retired memory")
	}
	rows, err := f.server.db.ListCheckpointObjects(t.Context(), pgvalue.UUID(uuid.MustParse(req.CheckpointID)))
	if err != nil || len(rows) != 0 {
		t.Fatalf("partial registration survived: %d %v", len(rows), err)
	}
	var empty bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT manifest IS NULL FROM run_checkpoints WHERE id=$1`, req.CheckpointID).Scan(&empty); err != nil || !empty {
		t.Fatalf("partial manifest survived: %v %v", empty, err)
	}
}

func TestCheckpointRegistrationRejectsWrongSource(t *testing.T) {
	f, req := checkpointRegistrationFixture(t)
	for _, change := range []func(*workerapi.RegisterCheckpointRequest){
		func(r *workerapi.RegisterCheckpointRequest) { r.RequestVersion++ },
		func(r *workerapi.RegisterCheckpointRequest) { r.Manifest.RecoveryPoint.RunID = uuid.NewV7().String() },
		func(r *workerapi.RegisterCheckpointRequest) {
			copy := *r.Manifest.RuntimeState.Computer
			copy.LogicalBytes /= 2
			r.Manifest.RuntimeState.Computer = &copy
		},
		func(r *workerapi.RegisterCheckpointRequest) {
			copy := *r.Manifest.RuntimeState.Computer
			copy.ComputerID = uuid.NewV7().String()
			r.Manifest.RuntimeState.Computer = &copy
		},
	} {
		changed := req
		change(&changed)
		if _, err := f.server.registerCheckpoint(context.Background(), f.worker, changed); err == nil {
			t.Fatal("accepted mismatched authority")
		}
	}
	rows, err := db.New(f.Pool).ListCheckpointObjects(t.Context(), pgvalue.UUID(uuid.MustParse(req.CheckpointID)))
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
			results := make(chan error, 2)
			for _, r := range []workerapi.RegisterCheckpointRequest{req, other} {
				go func() { <-start; _, err := f.server.registerCheckpoint(t.Context(), f.worker, r); results <- err }()
			}
			close(start)
			failures := 0
			for range 2 {
				if err := <-results; err != nil {
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
			rows, err := f.server.db.ListCheckpointObjects(t.Context(), pgvalue.UUID(uuid.MustParse(req.CheckpointID)))
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
	if err := f.Pool.QueryRow(ctx, `UPDATE run_checkpoints SET expires_at=clock_timestamp()+interval '3 seconds' WHERE id=$1 RETURNING expires_at`, req.CheckpointID).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := f.server.registerCheckpoint(ctx, f.worker, req); result <- err }()
	for {
		var blocked bool
		if err := f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, locker.Conn().PgConn().PID()).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("registration did not reach object lock: %v", err)
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
	if err := <-result; !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("expired registration: %v", err)
	}
	rows, err := f.server.db.ListCheckpointObjects(ctx, pgvalue.UUID(uuid.MustParse(req.CheckpointID)))
	if err != nil || len(rows) != 0 {
		t.Fatal("expired transaction retained a partial candidate")
	}
}

func TestCheckpointRegistrationCannotBypassPairedPublication(t *testing.T) {
	f, registered := checkpointRegistrationFixture(t)
	if _, err := f.server.registerCheckpoint(t.Context(), f.worker, registered); err != nil {
		t.Fatal(err)
	}
	ready := validCheckpointReadyRequest()
	ready.Lease = registered.Lease
	ready.RunWaitID = registered.RunWaitID
	ready.CheckpointID = registered.CheckpointID
	ready.RequestVersion = registered.RequestVersion
	ready.Manifest = registered.Manifest
	ready.Manifest.RuntimeState.Computer = nil
	if _, _, err := parseCheckpointReadyRequest(ready); err == nil {
		t.Fatal("accepted checkpoint without paired Computer disk")
	}
	rows, err := f.server.db.ListCheckpointObjects(t.Context(), pgvalue.UUID(uuid.MustParse(registered.CheckpointID)))
	if err != nil || len(rows) != 4 {
		t.Fatal("publication rejection lost candidate")
	}
	for _, row := range rows {
		if row.CheckpointStatus != "creating" || !row.AvailabilityRequired.Valid || !row.AvailabilityRequired.Bool {
			t.Fatalf("publication rejection released pin: %+v", row)
		}
	}
}
