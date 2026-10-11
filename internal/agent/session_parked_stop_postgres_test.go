package agent

import (
	"bytes"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSessionLifecycleStopsTerminalParkedProcess(t *testing.T) {
	for _, kind := range []string{"cancel", "close"} {
		for _, order := range []string{"park-before-control", "control-before-park"} {
			t.Run(kind+"/"+order, func(t *testing.T) {
				checkpoint := readyCheckpointFixture(t)
				f := checkpoint.f
				ctx := t.Context()
				park := func() {
					t.Helper()
					if err := ObserveComputerStopped(ctx, f.pool, *f.host(), ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, InstanceID: f.computer, Epoch: 1}, uuid.Nil()); err != nil {
						t.Fatal(err)
					}
				}
				if order == "park-before-control" {
					park()
				}
				if _, err := ControlSession(ctx, f.pool, f.caller(), controlRequest(f, kind, "terminal")); err != nil {
					t.Fatal(err)
				}
				if order == "control-before-park" {
					if _, _, err := reconcileSessionLifecycle(ctx, f.pool, sessionLifecyclePosition{}); err != nil {
						t.Fatal(err)
					}
					assertParkedProcessStopped(t, f, false)
					park()
				}
				var before []byte
				if err := f.pool.QueryRow(ctx, `SELECT manifest FROM computer_checkpoints WHERE id=$1`, checkpoint.manifest.CheckpointID).Scan(&before); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if _, _, err := reconcileSessionLifecycle(ctx, f.pool, sessionLifecyclePosition{}); err != nil {
						t.Fatal(err)
					}
				}
				assertParkedProcessStopped(t, f, true)
				var peerReady bool
				if err := f.pool.QueryRow(ctx, `SELECT bool_and(status='ready' AND fenced_at IS NULL AND failure_recorded_at IS NULL) FROM session_processes WHERE computer_id=$1 AND session_id<>$2`, f.computer, f.session).Scan(&peerReady); err != nil || !peerReady {
					t.Fatalf("peer altered: %v %v", peerReady, err)
				}
				var after []byte
				var ready bool
				if err := f.pool.QueryRow(ctx, `SELECT manifest,status='ready' FROM computer_checkpoints WHERE id=$1`, checkpoint.manifest.CheckpointID).Scan(&after, &ready); err != nil || !ready || !bytes.Equal(before, after) {
					t.Fatalf("shared checkpoint altered: %v %v", ready, err)
				}
				var events int
				if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM session_events WHERE session_id=$1 AND kind='session.process_stopped'`, f.session).Scan(&events); err != nil || events != 1 {
					t.Fatalf("stop events: %d %v", events, err)
				}
				// The healthy peer can restore the same checkpoint, but the terminal
				// captured member must remain in the guest's mandatory stop set.
				restored := restoreCheckpointFixture(t, checkpoint)
				installation := restored.prepare(t)
				if len(installation.StoppedSessions) != 1 || installation.StoppedSessions[0].SessionId != f.session.String() {
					t.Fatal("terminal captured member omitted from restore stop set")
				}
				if err := ValidateComputerRestore(ctx, f.pool, restored.host, f.env, installation, restoreReceipt(installation, false, false)); err != nil {
					t.Fatal(err)
				}
				if err := CommitComputerRestore(ctx, f.pool, restored.host, f.env, installation, restoreReceipt(installation, true, false)); err != nil {
					t.Fatal(err)
				}
				acknowledgeComputerMembers(t, f, restored.host, installation)
				if err := CompleteComputerRestore(ctx, f.pool, restored.host, f.env, installation, restoreReceipt(installation, true, true)); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestSessionLifecycleRequiresPhysicalStopForParkedControl(t *testing.T) {
	for _, target := range []string{"unfenced-source", "acquiring-target", "lost-target", "prepared-target"} {
		t.Run(target, func(t *testing.T) {
			checkpoint := readyCheckpointFixture(t)
			f := checkpoint.f
			if target == "unfenced-source" {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='lost',expires_at=clock_timestamp()-interval '1 second' WHERE computer_id=$1`, f.computer)
			} else {
				restored := restoreCheckpointFixture(t, checkpoint)
				if target == "prepared-target" {
					restored.prepare(t)
				}
				if target == "lost-target" {
					dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='lost',expires_at=clock_timestamp()-interval '1 second' WHERE computer_id=$1 AND epoch=2`, f.computer)
				}
			}
			if _, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "cancel", "cancel")); err != nil {
				t.Fatal(err)
			}
			if _, _, err := reconcileSessionLifecycle(t.Context(), f.pool, sessionLifecyclePosition{}); err != nil {
				t.Fatal(err)
			}
			assertParkedProcessStopped(t, f, false)
			// Revisit durable control after physical cleanup; no second user control
			// or live attachment is needed to finish the accepted cancellation.
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='joined physical closure' WHERE computer_id=$1 AND fenced_at IS NULL`, f.computer)
			if _, _, err := reconcileSessionLifecycle(t.Context(), f.pool, sessionLifecyclePosition{}); err != nil {
				t.Fatal(err)
			}
			assertParkedProcessStopped(t, f, true)
		})
	}
}

func assertParkedProcessStopped(t *testing.T, f fixture, want bool) {
	t.Helper()
	var stopped bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='stopped' AND fenced_at IS NOT NULL AND failure_recorded_at IS NULL FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=1`, f.env, f.session).Scan(&stopped); err != nil || stopped != want {
		t.Fatalf("process stopped=%v want=%v: %v", stopped, want, err)
	}
}

func TestSessionLifecycleRechecksRestoreTargetAfterComputerLock(t *testing.T) {
	checkpoint := readyCheckpointFixture(t)
	f := checkpoint.f
	ctx := t.Context()
	if err := ObserveComputerStopped(ctx, f.pool, *f.host(), ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, InstanceID: f.computer, Epoch: 1}, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	if _, err := ControlSession(ctx, f.pool, f.caller(), controlRequest(f, "cancel", "cancel")); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// Hold the same Computer lock as restore allocation. The stop attempt has
	// already been discovered before this allocation becomes visible to it.
	if _, err = tx.Exec(ctx, `SELECT id FROM computers WHERE id=$1 FOR NO KEY UPDATE`, f.computer); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- reconcileTerminalSessionStop(ctx, f.pool, f.env, f.session) }()
	waitSessionLifecycleLock(t, f)
	if _, err = tx.Exec(ctx, `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,
 computer_instance_id,channel_credential_digest,restored_from_save_id,status,reserved_cpu_millis,reserved_memory_bytes,
 reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest)
 SELECT environment_id,computer_id,2,worker_host_id,worker_epoch,$2,channel_credential_digest,$3,'acquiring',
 reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest
 FROM computer_leases l WHERE computer_id=$1 AND epoch=1`, f.computer, uuid.NewV7(), checkpoint.save); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	assertParkedProcessStopped(t, f, false)
}
