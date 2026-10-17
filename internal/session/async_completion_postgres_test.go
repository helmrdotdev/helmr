package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/session"
)

func TestAsyncCompletionCoveragePrefixIsImmutableAcrossConcurrentResult(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	storage := newSaveStorageFixture(t, f)
	a, first := f.finalize(t, "first")
	b, second := peer.finalize(t, "second")
	third := f.enqueue(t, "concurrent")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := closeProcessingForTest(t.Context(), f.pool, f.execution(), third.TurnID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	selected, err := computer.NextSave(t.Context(), tx, *f.host(), saveIdentity(f), true, false, nil)
	if err != nil || selected == nil || selected.ID != first.ID {
		t.Fatalf("selection %+v %v", selected, err)
	}
	done := make(chan error, 1)
	go func() {
		outcome, err := session.RuntimeFinalize(t.Context(), f.pool, *f.host(), f.execution(), third.TurnID, json.RawMessage(`3`), "joined")
		if err == nil && outcome == nil {
			err = errors.New("completion still pending")
		}
		done <- err
	}()
	waitSessionLifecycleLock(t, f)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Ignore the first discovery response: the receipt cannot grow after the
	// concurrent result commits, even though the flush has not begun yet.
	retry, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, false, nil)
	if err != nil || retry == nil || *retry != *selected {
		t.Fatalf("lost discovery reply %+v %v", retry, err)
	}
	var maxSeq int64
	if err := f.pool.QueryRow(t.Context(), `SELECT coverage_max_seq FROM computer_saves WHERE id=$1`, first.ID).Scan(&maxSeq); err != nil || maxSeq != second.Sequence {
		t.Fatalf("prefix %d %v", maxSeq, err)
	}
	root, _ := storage.cut(t, 9)
	for range 2 {
		if err := storage.publisher.Capture(t.Context(), storage.ref(first.ID), root, "same capture receipt"); err != nil {
			t.Fatal(err)
		}
	}
	var publishedAt string
	for range 2 {
		if err := storage.publish(t, first.ID, root); err != nil {
			t.Fatal(err)
		}
		var observed string
		if err := f.pool.QueryRow(t.Context(), `SELECT published_at::text FROM computer_saves WHERE id=$1`, first.ID).Scan(&observed); err != nil {
			t.Fatal(err)
		}
		if publishedAt != "" && publishedAt != observed {
			t.Fatal("publication retry replaced its committed timestamp")
		}
		publishedAt = observed
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT
 (SELECT status='published' FROM computer_saves WHERE id=$1)
 AND (SELECT status='superseded' AND covered_by_save_id=$1 FROM computer_saves WHERE id=$2)
 AND (SELECT status='requested' AND coverage_max_seq IS NULL FROM computer_saves WHERE turn_id=$3)
 AND (SELECT count(*)=3 AND bool_and(status='completed') FROM turns WHERE id=ANY($4::uuid[]))`, first.ID, second.ID, third.TurnID, []uuid.UUID{a.TurnID, b.TurnID, third.TurnID}).Scan(&valid); err != nil || !valid {
		t.Fatalf("coverage/result witness %v %v", valid, err)
	}
	if err := storage.publisher.Capture(t.Context(), storage.ref(second.ID), root, "covered request retry"); !errors.Is(err, computer.ErrTerminal) {
		t.Fatalf("superseded capture %v", err)
	}
	next, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, false, nil)
	if err != nil || next == nil || next.Sequence != second.Sequence+1 {
		t.Fatalf("next uncovered %+v %v", next, err)
	}
}

func TestAsyncCompletionCheckpointSaveIsNotCoalesced(t *testing.T) {
	f := newFixture(t)
	storage := newSaveStorageFixture(t, f)
	_, save := f.finalize(t, "completed")
	root, _ := storage.cut(t, 4)
	if err := storage.publisher.Capture(t.Context(), storage.ref(save.ID), root, "disk cut"); err != nil {
		t.Fatal(err)
	}
	// A coherent checkpoint cannot start until the outstanding prefix publishes.
	if _, err := session.BeginComputerCapture(t.Context(), f.pool, *f.host(), f.captureRequest()); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("checkpoint crossed pending save %v", err)
	}
	if err := storage.publish(t, save.ID, root); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ := beginCapture(t, f, f.captureRequest())
	if pending, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, true, nil); err != nil || pending != nil {
		t.Fatalf("discovery stole coherent cut %+v %v", pending, err)
	}
	var isolated bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='requested' AND coverage_max_seq IS NULL AND covered_by_save_id IS NULL FROM computer_saves WHERE id=$1`, checkpoint.SaveID).Scan(&isolated); err != nil || !isolated {
		t.Fatalf("checkpoint witness %v %v", isolated, err)
	}
}

func TestComputerRecoveryGapHoldsIdleSessionsAndPreservesIndependentHolds(t *testing.T) {
	f := newFixture(t)
	idle := f.peer(t)
	// The peer has no live process but still shares the Computer's files.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1`, idle.session)
	independent := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','independent runtime crash')`, f.env, independent, f.session)
	// Complete before installing the independent hold in a separate peer Session.
	active := f.peer(t)
	turn, save := active.finalize(t, "uncovered")
	active.enqueue(t, "followup")
	for range 2 {
		if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), leaseIdentity(f), uuid.Nil()); err != nil {
			t.Fatal(err)
		}
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT
 (SELECT count(*)=3 FROM session_holds WHERE kind='recovery_gap' AND released_at IS NULL)
 AND (SELECT count(*)=3 FROM session_events WHERE kind='session.recovery_gap')
 AND (SELECT convert_from(data,'UTF8')::jsonb->'affectedTurnIds'=jsonb_build_array($1::uuid::text) FROM session_events WHERE session_id=$2 AND kind='session.recovery_gap')
 AND (SELECT convert_from(data,'UTF8')::jsonb->'affectedTurnIds'='[]'::jsonb FROM session_events WHERE session_id=$3 AND kind='session.recovery_gap')
 AND (SELECT status='completed' FROM turns WHERE id=$1)
 AND (SELECT status='failed' FROM computer_saves WHERE id=$4)
 AND (SELECT released_at IS NULL FROM session_holds WHERE id=$5)`, turn.TurnID, active.session, idle.session, save.ID, independent).Scan(&valid); err != nil || !valid {
		t.Fatalf("sticky gap witness %v %v", valid, err)
	}
	var hold uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM session_holds WHERE session_id=$1 AND kind='recovery_gap'`, f.session).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	req := controlRequest(f, "resume", "acknowledge-gap")
	req.HoldID = hold
	if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), req); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT released_at IS NULL FROM session_holds WHERE id=$1`, independent).Scan(&valid); err != nil || !valid {
		t.Fatalf("resume changed independent hold %v %v", valid, err)
	}
}

func TestAcknowledgedRecoveryGapSurvivesPhysicalFenceAndNewEpochLoss(t *testing.T) {
	f := newFixture(t)
	checkpointImageExposure(t, f)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET fenced_at=clock_timestamp(),fence_evidence='fixture closure' WHERE environment_id=$1`, f.env)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET primary_pool_id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1) WHERE id=$2`, f.worker, f.group)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET observed_at=clock_timestamp(),epoch_guest_ephemeral_disk_bytes=1073741824 WHERE id=$1`, f.worker)
	f.finalize(t, "first-epoch-result")
	queued := f.enqueue(t, "new-epoch-result")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE computer_id=$1 AND epoch=1`, f.computer)
	if err := dispatch.ExpireLease(t.Context(), f.pool, f.env, f.computer, 1); err != nil {
		t.Fatal(err)
	}
	var hold uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM session_holds WHERE session_id=$1 AND kind='recovery_gap' AND released_at IS NULL`, f.session).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='lost' AND fenced_at IS NULL FROM computer_leases WHERE computer_id=$1 AND epoch=1`, f.computer).Scan(&valid); err != nil || !valid {
		t.Fatalf("expiry fenced physical writer %v %v", valid, err)
	}
	req := controlRequest(f, "resume", "acknowledge-before-physical-fence")
	req.HoldID = hold
	if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), req); err != nil {
		t.Fatal(err)
	}
	// Descriptive JSON cannot change the identity of an acknowledged loss.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_holds SET recovery_gap=recovery_gap || jsonb_build_object('computerId',$2::uuid::text,'leaseEpoch',0) WHERE id=$1`, hold, uuid.NewV7())
	if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), leaseIdentity(f), uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT
 (SELECT released_at IS NOT NULL FROM session_holds WHERE id=$1)
 AND (SELECT count(*)=1 FROM session_holds WHERE session_id=$2 AND kind='recovery_gap')
 AND (SELECT count(*)=1 FROM session_events WHERE session_id=$2 AND kind='session.recovery_gap')`, hold, f.session).Scan(&valid); err != nil || !valid {
		t.Fatalf("physical fence recreated acknowledged gap %v %v", valid, err)
	}
	allocator, err := dispatch.NewAllocator(f.pool, make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := allocator.AllocateRecoveredLease(t.Context(), f.env, f.computer, 2, f.worker)
	if err != nil || !recovered.DiskOnlyRecovery {
		t.Fatalf("acknowledged recovery %+v %v", recovered, err)
	}
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, Epoch: 2, InstanceID: recovered.InstanceID}
	if _, err := allocator.DeliverLease(t.Context(), *f.host(), identity); err != nil {
		t.Fatal(err)
	}
	if err := dispatch.ObserveFreshLeaseReady(t.Context(), f.pool, *f.host(), identity, recovered.Shape, recovered.BaseVersion); err != nil {
		t.Fatal(err)
	}
	e := f.execution()
	e.LeaseEpoch, e.ProcessEpoch = 2, 2
	if err := f.pool.QueryRow(t.Context(), `SELECT authority_generation FROM sessions WHERE id=$1`, f.session).Scan(&e.AuthorityGeneration); err != nil {
		t.Fatal(err)
	}
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	acknowledgeAdmissionControl(t, f, e, attachment.Sequence)
	next, err := session.DispatchRuntimeTurn(t.Context(), f.pool, *f.host(), e, attachment.Sequence)
	if err != nil || next == nil || next.TurnID != queued.TurnID {
		t.Fatalf("new epoch dispatch %+v %v", next, err)
	}
	if err := session.RuntimeCloseProcessing(t.Context(), f.pool, *f.host(), e, next.TurnID); err != nil {
		t.Fatal(err)
	}
	if result, err := session.RuntimeFinalize(t.Context(), f.pool, *f.host(), e, next.TurnID, json.RawMessage(`2`), "new epoch handler returned"); err != nil || result == nil {
		t.Fatalf("new epoch completion %s %v", result, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE computer_id=$1 AND epoch=2`, f.computer)
	if err := dispatch.ExpireLease(t.Context(), f.pool, f.env, f.computer, 2); err != nil {
		t.Fatal(err)
	}
	if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT
 (SELECT released_at IS NOT NULL FROM session_holds WHERE id=$1)
 AND (SELECT count(*)=2 FROM session_holds WHERE session_id=$2 AND kind='recovery_gap')
 AND (SELECT count(*)=1 FROM session_holds WHERE session_id=$2 AND kind='recovery_gap' AND released_at IS NULL AND recovery_lease_epoch=2 AND recovery_gap->'affectedTurnIds'=jsonb_build_array($3::uuid::text))
 AND (SELECT count(*)=2 FROM session_events WHERE session_id=$2 AND kind='session.recovery_gap')`, hold, f.session, queued.TurnID).Scan(&valid); err != nil || !valid {
		t.Fatalf("new epoch loss suppressed or duplicated %v %v", valid, err)
	}
}

func TestComputerNoGapRecoveryCreatesNewLeaseAndProcessWithoutReplay(t *testing.T) {
	f := newFixture(t)
	storage := newSaveStorageFixture(t, f)
	checkpointImageExposure(t, f)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET fenced_at=clock_timestamp(),fence_evidence='fixture closure' WHERE environment_id=$1`, f.env)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET primary_pool_id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1) WHERE id=$2`, f.worker, f.group)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET observed_at=clock_timestamp(),epoch_guest_ephemeral_disk_bytes=1073741824 WHERE id=$1`, f.worker)
	completed, save := f.finalize(t, "completed")
	root, _ := storage.cut(t, 5)
	if err := storage.publisher.Capture(t.Context(), storage.ref(save.ID), root, "saved cut"); err != nil {
		t.Fatal(err)
	}
	if err := storage.publish(t, save.ID, root); err != nil {
		t.Fatal(err)
	}
	queued := f.enqueue(t, "queued")
	if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), leaseIdentity(f), uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	var holds int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE released_at IS NULL`).Scan(&holds); err != nil || holds != 0 {
		t.Fatalf("no-gap hold %d %v", holds, err)
	}
	allocator, err := dispatch.NewAllocator(f.pool, make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := allocator.AllocateRecoveredLease(t.Context(), f.env, f.computer, 2, f.worker)
	if err != nil || !recovered.DiskOnlyRecovery || recovered.RestoredFrom == nil || *recovered.RestoredFrom != save.ID || recovered.BaseVersion != save.ID.String() {
		t.Fatalf("recovery %+v %v", recovered, err)
	}
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, Epoch: 2, InstanceID: recovered.InstanceID}
	if _, err := allocator.DeliverLease(t.Context(), *f.host(), identity); err != nil {
		t.Fatal(err)
	}
	if err := dispatch.ObserveFreshLeaseReady(t.Context(), f.pool, *f.host(), identity, recovered.Shape, recovered.BaseVersion); err != nil {
		t.Fatal(err)
	}
	e := f.execution()
	e.LeaseEpoch = 2
	e.ProcessEpoch = 2
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	acknowledgeAdmissionControl(t, f, e, attachment.Sequence)
	next, err := session.DispatchRuntimeTurn(t.Context(), f.pool, *f.host(), e, attachment.Sequence)
	if err != nil || next == nil || next.TurnID != queued.TurnID {
		t.Fatalf("recovered dispatch %+v %v", next, err)
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='completed' AND process_epoch=1 AND (SELECT count(*)=1 FROM session_events WHERE turn_id=$1 AND kind='turn.running') FROM turns WHERE id=$1`, completed.TurnID).Scan(&valid); err != nil || !valid {
		t.Fatalf("completed handler replayed %v %v", valid, err)
	}
}

func TestComputerLossPreservesAttentionOnlyForIneligibleDiskRecovery(t *testing.T) {
	for _, state := range []string{"initial", "published", "capacity", "integrity", "deleted", "revoked", "missing-image", "retired-head"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t)
			storage := newSaveStorageFixture(t, f)
			preparation := checkpointImageExposure(t, f)
			if state == "published" || state == "retired-head" {
				_, save := f.finalize(t, "covered-result")
				root, _ := storage.cut(t, 3)
				if err := storage.publisher.Capture(t.Context(), storage.ref(save.ID), root, "captured"); err != nil {
					t.Fatal(err)
				}
				if err := storage.publish(t, save.ID, root); err != nil {
					t.Fatal(err)
				}
				if state == "retired-head" {
					dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_saves SET root_id=NULL,payload_retired_at=clock_timestamp() WHERE id=$1`, save.ID)
				}
			}
			switch state {
			case "capacity":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_pools SET status='draining' WHERE worker_group_id=$1`, f.group)
			case "integrity":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET integrity_fault_at=clock_timestamp(),integrity_fault_reason='fixture fault' WHERE id=$1`, f.computer)
			case "deleted":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET deleted_at=clock_timestamp() WHERE id=$1`, f.computer)
			case "revoked":
				if _, err := preparation.secrets.Revoke(t.Context(), audit.System("test-fixture"), f.env, preparation.secretID, "revoke"); err != nil {
					t.Fatal(err)
				}
			case "missing-image":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET image_id=NULL WHERE id=$1`, f.computer)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE computer_id=$1`, f.computer)
			if err := dispatch.ExpireLease(t.Context(), f.pool, f.env, f.computer, 1); err != nil {
				t.Fatal(err)
			}
			want := 1
			if state == "initial" || state == "published" || state == "capacity" {
				want = 0
			}
			check := func() {
				t.Helper()
				var count int
				if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE session_id=$1 AND released_at IS NULL`, f.session).Scan(&count); err != nil || count != want {
					t.Fatalf("attention holds=%d want=%d error=%v", count, want, err)
				}
			}
			// Expiry is still physically unfenced, and capacity may be absent.
			check()
			if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), leaseIdentity(f), uuid.Nil()); err != nil {
				t.Fatal(err)
			}
			check()
		})
	}
}

func TestComputerLossInterruptionCauseOnlyDescribesRunningLostTurn(t *testing.T) {
	for _, state := range []string{"no-gap", "gap", "user-interrupted", "pressure-stopped"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t)
			checkpointImageExposure(t, f)
			if state == "gap" {
				f.finalize(t, "completed-with-unsaved-files")
			}
			running := f.enqueue(t, "running")
			if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
				t.Fatal(err)
			}
			queued := f.enqueue(t, "queued")
			if state == "user-interrupted" {
				req := controlRequest(f, "turn_interrupt", "user-stop")
				req.TurnID = running.TurnID
				if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), req); err != nil {
					t.Fatal(err)
				}
			}
			if state == "pressure-stopped" {
				if err := session.ReportStoragePressure(t.Context(), f.pool, *f.host(), saveIdentity(f), true); err != nil {
					t.Fatal(err)
				}
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET storage_pressure_since=clock_timestamp()-interval '301 seconds' WHERE computer_id=$1`, f.computer)
				if _, _, err := session.ReconcileSessionLifecycle(t.Context(), f.pool, session.SessionLifecyclePosition{}); err != nil {
					t.Fatal(err)
				}
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE computer_id=$1`, f.computer)
			if err := dispatch.ExpireLease(t.Context(), f.pool, f.env, f.computer, 1); err != nil {
				t.Fatal(err)
			}
			if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), leaseIdentity(f), uuid.Nil()); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := f.pool.QueryRow(t.Context(), `SELECT data FROM session_events WHERE session_id=$1 AND turn_id=$2 AND kind='turn.interrupted'`, f.session, running.TurnID).Scan(&raw); err != nil {
				t.Fatalf("missing or duplicated interruption: %v", err)
			}
			var cause session.ComputerLossInterruption
			if err := json.Unmarshal(raw, &cause); err != nil {
				t.Fatal(err)
			}
			if state == "user-interrupted" || state == "pressure-stopped" {
				if string(raw) != "{}" {
					t.Fatalf("ordinary interruption was relabelled as loss: %s", raw)
				}
			} else if cause.Reason != session.ComputerExecutionLost || cause.ComputerID != f.computer || cause.LeaseEpoch != 1 || cause.RecoveryGap != (state == "gap") {
				t.Fatalf("loss cause %+v", cause)
			}
			var valid bool
			if err := f.pool.QueryRow(t.Context(), `SELECT
 (SELECT count(*)=1 FROM session_events WHERE turn_id=$1 AND kind='turn.interrupted')
 AND (SELECT status='interrupted' FROM turns WHERE id=$1)
 AND (SELECT status='queued' FROM turns WHERE id=$2)
 AND NOT EXISTS(SELECT 1 FROM session_holds WHERE session_id=$3 AND kind='control' AND released_at IS NULL)`, running.TurnID, queued.TurnID, f.session).Scan(&valid); err != nil || !valid {
				t.Fatalf("loss replayed work or added attention hold: %v %v", valid, err)
			}
		})
	}
}

func TestInitialRootRecoveryKeepsOldEpochGapAndRetainedMetadata(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	storage := newSaveStorageFixture(t, f)
	checkpointImageExposure(t, f)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET fenced_at=clock_timestamp(),fence_evidence='fixture closure' WHERE environment_id=$1`, f.env)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET primary_pool_id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1) WHERE id=$2`, f.worker, f.group)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET observed_at=clock_timestamp(),epoch_guest_ephemeral_disk_bytes=1073741824 WHERE id=$1`, f.worker)
	lost, failed := f.finalize(t, "lost-first-result")
	f.enqueue(t, "continue-after-acknowledgment")
	for range 2 {
		if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), leaseIdentity(f), uuid.Nil()); err != nil {
			t.Fatal(err)
		}
	}
	view, err := session.GetSession(t.Context(), f.pool, f.caller(), f.env, f.session)
	if err != nil || len(view.Holds) != 1 || view.Holds[0].RecoveryGap == nil {
		t.Fatalf("durable gap view %+v %v", view, err)
	}
	metadata := view.Holds[0].RecoveryGap
	if metadata.HoldID != view.Holds[0].ID || metadata.ComputerID != f.computer || metadata.LeaseEpoch != 1 || metadata.RecoverySaveID != nil || metadata.RestoredAt != nil || len(metadata.AffectedTurnIDs) != 1 || metadata.AffectedTurnIDs[0] != lost.TurnID {
		t.Fatalf("initial environment metadata %+v", metadata)
	}
	var notices int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_events WHERE kind='session.recovery_gap'`).Scan(&notices); err != nil || notices != 2 {
		t.Fatalf("repeated loss duplicated notices %d %v", notices, err)
	}
	allocator, err := dispatch.NewAllocator(f.pool, make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := allocator.AllocateRecoveredLease(t.Context(), f.env, f.computer, 2, f.worker); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("recovery bypassed acknowledgment %v", err)
	}
	req := controlRequest(f, "resume", "acknowledge-initial-state")
	req.HoldID = view.Holds[0].ID
	if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), req); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_specs SET seed=jsonb_build_object('profile',$2::text) WHERE environment_id=$1`, f.env, definition.ComputerSeedProfile)
	// Expiring the event stream cannot discard unresolved hold-owned facts.
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM session_events WHERE kind='session.recovery_gap'`)
	admitted, err := session.Start(t.Context(), f.pool, nil, f.caller(), session.StartRequest{EnvironmentID: f.env, Agent: "agent", ComputerID: f.computer, IdempotencyKey: "joined-after-loss", Input: json.RawMessage(`[{"type":"text","text":"continue"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	joined, err := session.GetSession(t.Context(), f.pool, f.caller(), f.env, admitted.SessionID)
	if err != nil || len(joined.Holds) != 1 || joined.Holds[0].RecoveryGap == nil || len(joined.Holds[0].RecoveryGap.AffectedTurnIDs) != 0 || joined.Holds[0].RecoveryGap.HoldID != joined.Holds[0].ID || joined.Holds[0].RecoveryGap.ComputerID != f.computer || joined.Holds[0].RecoveryGap.LeaseEpoch != 1 {
		t.Fatalf("joined gap %+v %v", joined, err)
	}
	recovered, err := allocator.AllocateRecoveredLease(t.Context(), f.env, f.computer, 2, f.worker)
	if err != nil || !recovered.DiskOnlyRecovery || recovered.RestoredFrom != nil {
		t.Fatalf("initial recovery %+v %v", recovered, err)
	}
	var original string
	if err := f.pool.QueryRow(t.Context(), `SELECT 'sha256:'||encode(initial_root_digest,'hex') FROM computers WHERE id=$1`, f.computer).Scan(&original); err != nil || recovered.BaseVersion != original {
		t.Fatalf("changed initial root %s %v", recovered.BaseVersion, err)
	}
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, Epoch: 2, InstanceID: recovered.InstanceID}
	if _, err := allocator.DeliverLease(t.Context(), *f.host(), identity); err != nil {
		t.Fatal(err)
	}
	if err := dispatch.ObserveFreshLeaseReady(t.Context(), f.pool, *f.host(), identity, recovered.Shape, recovered.BaseVersion); err != nil {
		t.Fatal(err)
	}
	var key uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT write_key_id FROM computer_leases WHERE computer_id=$1 AND epoch=1`, f.computer).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if _, err := bindComputerLeaseDiskForTest(t.Context(), f.pool, *f.host(), f.env, f.computer, 2, key); err != nil {
		t.Fatal(err)
	}
	next, err := computer.NextSave(t.Context(), f.pool, *f.host(), identity, true, true, nil)
	if err != nil || next == nil {
		t.Fatalf("new epoch discovery %+v %v", next, err)
	}
	locator, err := storage.writer.Capture(t.Context(), storage.base, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	root, err := disk.NewVersionRoot(locator, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ref := computer.SavePublication{EnvironmentID: f.env, SaveID: next.ID, LeaseEpoch: 2, Host: *f.host()}
	if err := storage.publisher.Capture(t.Context(), ref, root, "new epoch cut"); err != nil {
		t.Fatal(err)
	}
	storage.certify(t, ref, root)
	if err := storage.publisher.Publish(t.Context(), ref, root, "new epoch publication"); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT
 (SELECT status='failed' AND covered_by_save_id IS NULL FROM computer_saves WHERE id=$1)
 AND (SELECT count(*)=1 FROM session_holds WHERE session_id=$2 AND kind='recovery_gap' AND released_at IS NULL)
 AND (SELECT recovery_gap->'affectedTurnIds'='[]'::jsonb FROM session_holds WHERE session_id=$2 AND kind='recovery_gap' AND released_at IS NULL)
 AND (SELECT status='completed' FROM turns WHERE id=$3)`, failed.ID, peer.session, lost.TurnID).Scan(&valid); err != nil || !valid {
		t.Fatalf("new epoch erased gap %v %v", valid, err)
	}
}

func TestRecoveryDoesNotSubstituteInitialRootForUnavailablePublishedHead(t *testing.T) {
	f := newFixture(t)
	storage := newSaveStorageFixture(t, f)
	checkpointImageExposure(t, f)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET fenced_at=clock_timestamp(),fence_evidence='fixture closure' WHERE environment_id=$1`, f.env)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET primary_pool_id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1) WHERE id=$2`, f.worker, f.group)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET observed_at=clock_timestamp(),epoch_guest_ephemeral_disk_bytes=1073741824 WHERE id=$1`, f.worker)
	_, save := f.finalize(t, "published-head")
	root, _ := storage.cut(t, 7)
	if err := storage.publisher.Capture(t.Context(), storage.ref(save.ID), root, "captured"); err != nil {
		t.Fatal(err)
	}
	if err := storage.publish(t, save.ID, root); err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, "pending")
	if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), leaseIdentity(f), uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_saves SET root_id=NULL,payload_retired_at=clock_timestamp() WHERE id=$1`, save.ID)
	allocator, err := dispatch.NewAllocator(f.pool, make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := allocator.AllocateRecoveredLease(t.Context(), f.env, f.computer, 2, f.worker); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("unavailable newer head fell back to initial state %v", err)
	}
	var leases int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_leases WHERE computer_id=$1`, f.computer).Scan(&leases); err != nil || leases != 1 {
		t.Fatalf("failed admission left lease %d %v", leases, err)
	}
}
