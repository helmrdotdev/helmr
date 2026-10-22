package session_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/jackc/pgx/v5"
)

type checkpointStorageFixture struct {
	*saveStorageFixture
	ref      session.CheckpointPublication
	manifest computercheckpoint.Manifest
	save     uuid.UUID
}

func newCheckpointStorageFixture(t *testing.T) checkpointStorageFixture {
	t.Helper()
	return checkpointStorageForFixture(t, newFixture(t))
}

func checkpointStorageForFixture(t *testing.T, f fixture) checkpointStorageFixture {
	t.Helper()
	return checkpointStorageForSaveFixture(t, newSaveStorageFixture(t, f))
}

func checkpointStorageForSaveFixture(t *testing.T, s *saveStorageFixture) checkpointStorageFixture {
	return checkpointStorageWithMembers(t, s, true)
}

func checkpointStorageWithMembers(t *testing.T, s *saveStorageFixture, members bool) checkpointStorageFixture {
	t.Helper()
	f := s.f
	if members {
		f.peer(t)
	}
	req := f.captureRequest()
	capture, p := beginCapture(t, f, req)
	if err := session.RecordComputerSealed(t.Context(), f.pool, *f.host(), f.env, req.CheckpointID, &agentv1.ComputerSessionReceipt{CheckpointId: req.CheckpointID.String(), DesiredVersion: p.DesiredVersion, Frozen: true}); err != nil {
		t.Fatal(err)
	}
	root, identity := s.cut(t, 42)
	if err := recordCaptureForTest(t.Context(), f.pool, computer.CaptureEvidence{EnvironmentID: f.env, SaveID: capture.SaveID, LeaseEpoch: 1, DiskRoot: identity, Evidence: "owned coherent VM capture"}); err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"config":"captured"}`)
	digest := "sha256:" + strings.Repeat("1", 64)
	captureDigest := sha256.Sum256(capture.Request)
	m := computercheckpoint.Manifest{MemoryHeaderDigest: sha256sum.DigestBytes([]byte("header")), CheckpointID: req.CheckpointID, ComputerID: f.computer, InstanceID: f.computer, LeaseEpoch: 1, ControlVersion: p.DesiredVersion, CaptureDigest: captureDigest[:], Config: config, Disk: root, Runtime: vm.CheckpointIdentity{RuntimeBackend: "firecracker", RuntimeArch: "x86_64", VMRuntimeContract: "helmr.vm-runtime.v0", RuntimeID: digest, KernelDigest: digest, InitramfsDigest: digest, RootfsDigest: digest, VMConfigDigest: sha256sum.DigestBytes(config), VMVCPUCount: 1, CPUConfigDigest: digest}}
	for _, v := range p.Sessions {
		id, err := uuid.Parse(v.SessionId)
		if err != nil {
			t.Fatal(err)
		}
		m.Members = append(m.Members, computercheckpoint.Session{SessionID: id, ProcessEpoch: int64(v.ProcessEpoch)})
	}
	for i, media := range []string{cas.CheckpointVMConfigMediaType, cas.CheckpointVMStateMediaType, cas.CheckpointMemoryMediaType, cas.CheckpointScratchDiskMediaType} {
		o, err := s.local.Put(t.Context(), media, strings.NewReader("opaque captured ciphertext for "+media))
		if err != nil {
			t.Fatal(err)
		}
		value := computercheckpoint.Object{Digest: o.Digest, SizeBytes: o.SizeBytes, MediaType: o.MediaType}
		switch i {
		case 0:
			m.VMConfig = value
		case 1:
			m.VMState = value
		case 2:
			m.Memory = value
		case 3:
			m.ScratchDisk = value
		}
	}
	return checkpointStorageFixture{s, session.CheckpointPublication{EnvironmentID: f.env, Host: *f.host()}, m, capture.SaveID}
}
func (f checkpointStorageFixture) upload(t *testing.T) {
	t.Helper()
	for _, o := range f.manifest.Objects() {
		file, err := f.local.Get(t.Context(), o.Digest)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.remote.Put(t.Context(), o.MediaType, file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
}
func TestCheckpointRuntimeReadinessRetainsCompleteImage(t *testing.T) {
	f := newCheckpointStorageFixture(t)
	ctx := t.Context()
	if err := f.checkpoints.CompleteCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("unregistered: %v", err)
	}
	if err := f.checkpoints.RegisterCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	if err := f.checkpoints.CompleteCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, computer.ErrSaveStorageUnavailable) {
		t.Fatalf("missing upload: %v", err)
	}
	if _, err := f.f.pool.Exec(ctx, `UPDATE cas_blobs SET retired_at=clock_timestamp(),next_reclaim_at=clock_timestamp() WHERE digest=$1`, f.manifest.Memory.Digest); err == nil {
		t.Fatal("memory pin did not prevent reclamation")
	}
	f.upload(t)
	if err := f.checkpoints.CompleteCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("unpublished disk: %v", err)
	}
	if err := f.publish(t, f.save, f.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	// Recreating the publisher retains exact operation identity through DB/CAS.
	restarted, err := session.NewCheckpointPublisher(f.f.pool, f.remote)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.CompleteCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = f.f.pool.QueryRow(ctx, `SELECT status FROM checkpoints WHERE id=$1`, f.manifest.CheckpointID).Scan(&status); err != nil || status != "ready" {
		t.Fatalf("status=%s %v", status, err)
	}
	// Ready does not dispatch the still-sealed source, including newly queued work.
	f.f.enqueue(t, "queued-while-ready")
	if _, err = dispatchTurnForTest(ctx, f.f.pool, f.f.execution()); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("ready dispatched: %v", err)
	}
	dbtest.MustExec(t, ctx, f.f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE computer_id=$1`, f.f.computer)
	unavailable, _ := session.NewCheckpointPublisher(f.f.pool, saveStatFunc(func(context.Context, string) (cas.Object, error) {
		t.Fatal("committed receipt consulted storage")
		return cas.Object{}, nil
	}))
	if err = unavailable.CompleteCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatalf("lost ready acknowledgement: %v", err)
	}
	reordered := f.manifest
	reordered.Members = append([]computercheckpoint.Session(nil), reordered.Members...)
	reordered.Members[0], reordered.Members[1] = reordered.Members[1], reordered.Members[0]
	if err = unavailable.RegisterCheckpoint(ctx, f.ref, reordered); err != nil {
		t.Fatalf("equivalent registration: %v", err)
	}
	changed := f.manifest
	changed.VMState.SizeBytes++
	if err = unavailable.CompleteCheckpoint(ctx, f.ref, changed); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("changed ready receipt: %v", err)
	}
}
func TestCheckpointRuntimeRejectsMismatchedCapture(t *testing.T) {
	for _, which := range []string{"instance", "members", "capture", "root", "runtime", "config", "media"} {
		t.Run(which, func(t *testing.T) {
			f := newCheckpointStorageFixture(t)
			m := f.manifest
			switch which {
			case "instance":
				m.InstanceID = uuid.NewV7()
			case "members":
				m.Members = append([]computercheckpoint.Session(nil), m.Members...)
				m.Members[0].ProcessEpoch++
			case "capture":
				m.CaptureDigest = bytes.Repeat([]byte{2}, 32)
			case "root":
				m.Disk, _ = f.cut(t, 43)
			case "runtime":
				m.Runtime.KernelDigest = sha256sum.DigestBytes([]byte("other"))
			case "config":
				m.Config = []byte(`{}`)
			case "media":
				m.Memory.MediaType = cas.CheckpointVMStateMediaType
			}
			expected := session.ErrConflict
			if which == "runtime" {
				expected = computer.ErrConflict
			}
			if which == "config" || which == "media" {
				expected = session.ErrInvalidInput
			}
			if err := f.checkpoints.RegisterCheckpoint(t.Context(), f.ref, m); !errors.Is(err, expected) {
				t.Fatalf("mismatch accepted: %v", err)
			}
			var pins int
			if err := f.f.pool.QueryRow(t.Context(), `SELECT count(*) FROM checkpoint_objects`).Scan(&pins); err != nil || pins != 0 {
				t.Fatalf("pins=%d %v", pins, err)
			}
		})
	}
}
func TestCheckpointRuntimeRechecksAuthorityAfterIO(t *testing.T) {
	f := newCheckpointStorageFixture(t)
	ctx := t.Context()
	if err := f.checkpoints.RegisterCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	f.upload(t)
	if err := f.publish(t, f.save, f.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	first := true
	blocked, _ := session.NewCheckpointPublisher(f.f.pool, saveStatFunc(func(ctx context.Context, digest string) (cas.Object, error) {
		if first {
			first = false
			close(entered)
			<-release
		}
		return f.remote.Stat(ctx, digest)
	}))
	done := make(chan error, 1)
	go func() { done <- blocked.CompleteCheckpoint(ctx, f.ref, f.manifest) }()
	<-entered
	// This update must complete while object storage is blocked.
	dbtest.MustExec(t, ctx, f.f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE computer_id=$1`, f.f.computer)
	close(release)
	if err := <-done; !errors.Is(err, session.ErrDenied) {
		t.Fatalf("expired publication: %v", err)
	}
	var ready bool
	if err := f.f.pool.QueryRow(ctx, `SELECT ready_at IS NOT NULL FROM checkpoints WHERE id=$1`, f.manifest.CheckpointID).Scan(&ready); err != nil || ready {
		t.Fatalf("ready=%v %v", ready, err)
	}
	if err := f.checkpoints.RegisterCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("expired register: %v", err)
	}
}

func TestCheckpointRuntimeForeignReceiptsAndAbortRace(t *testing.T) {
	for _, action := range []string{"abort", "other-host", "replacement-epoch", "changed-descriptor"} {
		t.Run(action, func(t *testing.T) {
			f := newCheckpointStorageFixture(t)
			ctx := t.Context()
			if err := f.checkpoints.RegisterCheckpoint(ctx, f.ref, f.manifest); err != nil {
				t.Fatal(err)
			}
			f.upload(t)
			if err := f.publish(t, f.save, f.manifest.Disk); err != nil {
				t.Fatal(err)
			}
			if action == "abort" {
				// The source-abort transaction and readiness contend on the checkpoint.
				// Use the retained request to exercise the real abort arbitration.
				p, _ := sourceAbortForCheckpoint(t, f.f, f.manifest.CheckpointID)
				if err := session.ValidateComputerSourceAbort(ctx, f.f.pool, f.ref.Host, f.f.env, p, abortReceipt(p, false, false)); err != nil {
					t.Fatal(err)
				}
				if err := f.checkpoints.CompleteCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, session.ErrNotReady) {
					t.Fatalf("aborting image became ready: %v", err)
				}
				return
			}
			if action == "changed-descriptor" {
				wrong, _ := session.NewCheckpointPublisher(f.f.pool, saveStatFunc(func(ctx context.Context, d string) (cas.Object, error) {
					o, e := f.remote.Stat(ctx, d)
					o.SizeBytes++
					return o, e
				}))
				if err := wrong.CompleteCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, session.ErrConflict) {
					t.Fatalf("wrong remote descriptor: %v", err)
				}
				return
			}
			if err := f.checkpoints.CompleteCheckpoint(ctx, f.ref, f.manifest); err != nil {
				t.Fatal(err)
			}
			ref := f.ref
			if action == "other-host" {
				other := uuid.NewV7()
				dbtest.MustExec(t, ctx, f.f.pool, `INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$2::uuid,'resource_id','other-host','current_service_id',$2::uuid))).* FROM worker_hosts h WHERE id=$1`, f.f.worker, other)
				ref.Host.HostID = other
			} else {
				dbtest.MustExec(t, ctx, f.f.pool, `UPDATE worker_hosts SET current_epoch=2 WHERE id=$1`, f.f.worker)
				ref.Host.Epoch = 2
			}
			if err := f.checkpoints.RegisterCheckpoint(ctx, ref, f.manifest); !errors.Is(err, session.ErrDenied) {
				t.Fatalf("foreign registration: %v", err)
			}
			if err := f.checkpoints.CompleteCheckpoint(ctx, ref, f.manifest); !errors.Is(err, session.ErrDenied) {
				t.Fatalf("foreign completion: %v", err)
			}
			wrong := f.manifest
			wrong.ComputerID = uuid.NewV7()
			wrong.LeaseEpoch++
			wrong.ControlVersion++
			if err := f.checkpoints.CompleteCheckpoint(ctx, ref, wrong); !errors.Is(err, session.ErrDenied) {
				t.Fatalf("foreign identity probing: %v", err)
			}
		})
	}
}

func TestCheckpointRuntimeReadyCannotReplayAfterAbort(t *testing.T) {
	f := newCheckpointStorageFixture(t)
	ctx := t.Context()
	if err := f.checkpoints.RegisterCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	f.upload(t)
	if err := f.publish(t, f.save, f.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	if err := f.checkpoints.CompleteCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	p, _ := sourceAbortForCheckpoint(t, f.f, f.manifest.CheckpointID)
	if _, err := session.ControlComputerExecution(ctx, f.f.pool, f.f.caller(), f.f.executionRequest("wake", "resume-published-source")); err != nil {
		t.Fatal(err)
	}
	if err := session.ValidateComputerSourceAbort(ctx, f.f.pool, f.ref.Host, f.f.env, p, abortReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"aborting", "consumed"} {
		if state == "consumed" {
			if err := session.CommitComputerSourceAbort(ctx, f.f.pool, f.ref.Host, f.f.env, p, abortReceipt(p, true, false)); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.checkpoints.RegisterCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, session.ErrNotReady) {
			t.Fatalf("%s registration replay: %v", state, err)
		}
		if err := f.checkpoints.CompleteCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, session.ErrNotReady) {
			t.Fatalf("%s ready replay: %v", state, err)
		}
	}
}

func TestCheckpointRuntimeAbortDuringRemoteVerification(t *testing.T) {
	f := newCheckpointStorageFixture(t)
	ctx := t.Context()
	if err := f.checkpoints.RegisterCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	f.upload(t)
	if err := f.publish(t, f.save, f.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	p, _ := sourceAbortForCheckpoint(t, f.f, f.manifest.CheckpointID)
	entered, release := make(chan struct{}), make(chan struct{})
	first := true
	blocked, _ := session.NewCheckpointPublisher(f.f.pool, saveStatFunc(func(ctx context.Context, d string) (cas.Object, error) {
		if first {
			first = false
			close(entered)
			<-release
		}
		return f.remote.Stat(ctx, d)
	}))
	done := make(chan error, 1)
	go func() { done <- blocked.CompleteCheckpoint(ctx, f.ref, f.manifest) }()
	<-entered
	if err := session.ValidateComputerSourceAbort(ctx, f.f.pool, f.ref.Host, f.f.env, p, abortReceipt(p, false, false)); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("aborted candidate became ready: %v", err)
	}
	var state string
	var ready bool
	if err := f.f.pool.QueryRow(ctx, `SELECT status,ready_at IS NOT NULL FROM checkpoints WHERE id=$1`, f.manifest.CheckpointID).Scan(&state, &ready); err != nil || state != "aborting" || ready {
		t.Fatalf("state=%s ready=%v %v", state, ready, err)
	}
}

func TestCheckpointRuntimeRechecksLeaseAfterCASWriteWait(t *testing.T) {
	f := newCheckpointStorageFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := f.checkpoints.RegisterCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	f.upload(t)
	if err := f.publish(t, f.save, f.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	object := f.manifest.Objects()[0]
	dbtest.MustExec(t, ctx, f.f.pool, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$2,$3,$4 FROM environments WHERE id=$1 ON CONFLICT DO NOTHING`, f.f.env, object.Digest, object.SizeBytes, object.MediaType)
	holder, err := f.f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err = holder.Exec(ctx, `SELECT 1 FROM cas_objects WHERE digest=$1 FOR UPDATE`, object.Digest); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, f.f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()+interval '5 seconds' WHERE environment_id=$1 AND computer_id=$2`, f.f.env, f.f.computer)
	done := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		done <- f.checkpoints.CompleteCheckpoint(ctx, f.ref, f.manifest)
	}()
	defer func() { cancel(); holder.Rollback(context.Background()); wg.Wait() }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err = f.f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%INSERT INTO cas_objects%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("publication did not wait for CAS write: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	// The publication holds the lease lock. Let its actual database deadline
	// pass while the independent CAS row prevents readiness from committing.
	if _, err = f.f.pool.Exec(ctx, `SELECT pg_sleep(GREATEST(EXTRACT(EPOCH FROM expires_at-clock_timestamp()),0)) FROM computer_leases WHERE environment_id=$1 AND computer_id=$2`, f.f.env, f.f.computer); err != nil {
		t.Fatal(err)
	}
	if err = holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, session.ErrDenied) {
		t.Fatalf("expired authority published checkpoint: %v", err)
	}
	var ready bool
	if err = f.f.pool.QueryRow(ctx, `SELECT ready_at IS NOT NULL FROM checkpoints WHERE id=$1`, f.manifest.CheckpointID).Scan(&ready); err != nil || ready {
		t.Fatalf("ready=%v: %v", ready, err)
	}
}

func TestStandaloneComputerPublishesAndWakesWithoutSessions(t *testing.T) {
	base := newFixture(t)
	dbtest.MustExec(t, t.Context(), base.pool, `DELETE FROM session_processes WHERE environment_id=$1; DELETE FROM sessions WHERE environment_id=$1`, pgx.QueryExecModeSimpleProtocol, base.env)
	f := checkpointStorageWithMembers(t, newSaveStorageFixture(t, base), false)
	if len(f.manifest.Members) != 0 {
		t.Fatal("standalone checkpoint acquired Sessions")
	}
	if err := f.checkpoints.RegisterCheckpoint(t.Context(), f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	f.upload(t)
	if err := f.publish(t, f.save, f.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	if err := f.checkpoints.CompleteCheckpoint(t.Context(), f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ControlComputerExecution(t.Context(), base.pool, base.caller(), base.executionRequest("wake", "standalone-wake")); err != nil {
		t.Fatal(err)
	}
	observed := &agentv1.ComputerSessionReceipt{CheckpointId: f.manifest.CheckpointID.String(), DesiredVersion: f.manifest.ControlVersion, Frozen: true}
	p, err := session.PrepareComputerSourceAbort(t.Context(), base.pool, *base.host(), base.env, f.manifest.CheckpointID, 1, sessiontest.ChannelCredential, observed)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Grants) != 0 {
		t.Fatal("standalone continuation acquired Session grants")
	}
	if err := session.ValidateComputerSourceAbort(t.Context(), base.pool, *base.host(), base.env, p, observed); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), base.pool, `UPDATE computers SET execution_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, base.computer)
	if err := session.CommitComputerSourceAbort(t.Context(), base.pool, *base.host(), base.env, p, abortReceipt(p, true, false)); !errors.Is(err, computer.ErrNotReady) {
		t.Fatalf("expired continuation committed: %v", err)
	}
	dbtest.MustExec(t, t.Context(), base.pool, `UPDATE computers SET execution_deadline_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, base.computer)
	if err := session.CommitComputerSourceAbort(t.Context(), base.pool, *base.host(), base.env, p, abortReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	var pending bool
	if err := base.pool.QueryRow(t.Context(), `SELECT execution_deadline_at IS NOT NULL FROM computers WHERE id=$1`, base.computer).Scan(&pending); err != nil || pending {
		t.Fatalf("committed source retained preparation deadline: %v %v", pending, err)
	}
	if err := session.CompleteComputerSourceAbort(t.Context(), base.pool, *base.host(), base.env, p, abortReceipt(p, true, true)); err != nil {
		t.Fatal(err)
	}
}

func TestHibernateReceiptWaitsForOwnedPublication(t *testing.T) {
	f := newCheckpointStorageFixture(t)
	dbtest.MustExec(t, t.Context(), f.f.pool, `UPDATE computer_controls SET deadline_at=clock_timestamp()-interval '1 minute' WHERE computer_id=$1`, f.f.computer)
	if err := computer.ReconcileExecutionControls(t.Context(), f.f.pool); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.f.pool.QueryRow(t.Context(), `SELECT status FROM computer_controls WHERE computer_id=$1`, f.f.computer).Scan(&state); err != nil || state != "accepted" {
		t.Fatalf("active publication receipt=%s: %v", state, err)
	}
	if err := f.checkpoints.RegisterCheckpoint(t.Context(), f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	f.upload(t)
	if err := f.publish(t, f.save, f.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	if err := f.checkpoints.CompleteCheckpoint(t.Context(), f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	if err := f.f.pool.QueryRow(t.Context(), `SELECT status FROM computer_controls WHERE computer_id=$1`, f.f.computer).Scan(&state); err != nil || state != "completed" {
		t.Fatalf("published receipt=%s: %v", state, err)
	}
}
