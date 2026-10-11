package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type checkpointStorageFixture struct {
	*saveStorageFixture
	ref      CheckpointPublication
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
	t.Helper()
	f := s.f
	f.peer(t)
	req := f.captureRequest()
	capture, p := beginCapture(t, f, req)
	if err := RecordComputerSealed(t.Context(), f.pool, *f.host(), f.env, req.CheckpointID, &agentv1.ComputerSessionReceipt{CheckpointId: req.CheckpointID.String(), DesiredVersion: p.DesiredVersion, Frozen: true}); err != nil {
		t.Fatal(err)
	}
	root, identity := s.cut(t, 42)
	if err := RecordCapture(t.Context(), f.pool, CaptureEvidence{EnvironmentID: f.env, SaveID: capture.SaveID, LeaseEpoch: 1, DiskRoot: identity, Evidence: "owned coherent VM capture"}); err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"config":"captured"}`)
	digest := "sha256:" + strings.Repeat("1", 64)
	captureDigest := sha256.Sum256(capture.Request)
	m := computercheckpoint.Manifest{CheckpointID: req.CheckpointID, ComputerID: f.computer, InstanceID: f.computer, LeaseEpoch: 1, ControlVersion: p.DesiredVersion, CaptureDigest: captureDigest[:], Config: config, Disk: root, Runtime: vm.CheckpointIdentity{RuntimeBackend: "firecracker", RuntimeArch: "x86_64", VMRuntimeContract: "helmr.vm-runtime.v0", RuntimeID: digest, KernelDigest: digest, InitramfsDigest: digest, RootfsDigest: digest, VMConfigDigest: sha256sum.DigestBytes(config), VMVCPUCount: 1, CPUConfigDigest: digest}}
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
	return checkpointStorageFixture{s, CheckpointPublication{EnvironmentID: f.env, Host: *f.host()}, m, capture.SaveID}
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
	if err := f.publisher.CompleteCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unregistered: %v", err)
	}
	if err := f.publisher.RegisterCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	if err := f.publisher.CompleteCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, ErrSaveStorageUnavailable) {
		t.Fatalf("missing upload: %v", err)
	}
	if _, err := f.f.pool.Exec(ctx, `UPDATE cas_blobs SET retired_at=clock_timestamp(),next_reclaim_at=clock_timestamp() WHERE digest=$1`, f.manifest.Memory.Digest); err == nil {
		t.Fatal("memory pin did not prevent reclamation")
	}
	f.upload(t)
	if err := f.publisher.CompleteCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unpublished disk: %v", err)
	}
	if err := f.publish(t, f.save, f.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	// Recreating the publisher retains exact operation identity through DB/CAS.
	restarted, err := NewSavePublisher(f.f.pool, f.remote)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.CompleteCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = f.f.pool.QueryRow(ctx, `SELECT status FROM computer_checkpoints WHERE id=$1`, f.manifest.CheckpointID).Scan(&status); err != nil || status != "ready" {
		t.Fatalf("status=%s %v", status, err)
	}
	// Ready does not dispatch the still-sealed source, including newly queued work.
	f.f.enqueue(t, "queued-while-ready")
	if _, err = Dispatch(ctx, f.f.pool, f.f.execution()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("ready dispatched: %v", err)
	}
	dbtest.MustExec(t, ctx, f.f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE computer_id=$1`, f.f.computer)
	unavailable, _ := NewSavePublisher(f.f.pool, saveStatFunc(func(context.Context, string) (cas.Object, error) {
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
	if err = unavailable.CompleteCheckpoint(ctx, f.ref, changed); !errors.Is(err, ErrConflict) {
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
			expected := ErrConflict
			if which == "config" || which == "media" {
				expected = ErrInvalidInput
			}
			if err := f.publisher.RegisterCheckpoint(t.Context(), f.ref, m); !errors.Is(err, expected) {
				t.Fatalf("mismatch accepted: %v", err)
			}
			var pins int
			if err := f.f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoint_objects`).Scan(&pins); err != nil || pins != 0 {
				t.Fatalf("pins=%d %v", pins, err)
			}
		})
	}
}
func TestCheckpointRuntimeRechecksAuthorityAfterIO(t *testing.T) {
	f := newCheckpointStorageFixture(t)
	ctx := t.Context()
	if err := f.publisher.RegisterCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	f.upload(t)
	if err := f.publish(t, f.save, f.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	first := true
	blocked, _ := NewSavePublisher(f.f.pool, saveStatFunc(func(ctx context.Context, digest string) (cas.Object, error) {
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
	if err := <-done; !errors.Is(err, ErrDenied) {
		t.Fatalf("expired publication: %v", err)
	}
	var ready bool
	if err := f.f.pool.QueryRow(ctx, `SELECT ready_at IS NOT NULL FROM computer_checkpoints WHERE id=$1`, f.manifest.CheckpointID).Scan(&ready); err != nil || ready {
		t.Fatalf("ready=%v %v", ready, err)
	}
	if err := f.publisher.RegisterCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, ErrDenied) {
		t.Fatalf("expired register: %v", err)
	}
}

func TestCheckpointRuntimeForeignReceiptsAndAbortRace(t *testing.T) {
	for _, action := range []string{"abort", "other-host", "replacement-epoch", "changed-descriptor"} {
		t.Run(action, func(t *testing.T) {
			f := newCheckpointStorageFixture(t)
			ctx := t.Context()
			if err := f.publisher.RegisterCheckpoint(ctx, f.ref, f.manifest); err != nil {
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
				if err := ValidateComputerSourceAbort(ctx, f.f.pool, f.ref.Host, f.f.env, p, abortReceipt(p, false, false)); err != nil {
					t.Fatal(err)
				}
				if err := f.publisher.CompleteCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, ErrNotReady) {
					t.Fatalf("aborting image became ready: %v", err)
				}
				return
			}
			if action == "changed-descriptor" {
				wrong, _ := NewSavePublisher(f.f.pool, saveStatFunc(func(ctx context.Context, d string) (cas.Object, error) {
					o, e := f.remote.Stat(ctx, d)
					o.SizeBytes++
					return o, e
				}))
				if err := wrong.CompleteCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, ErrConflict) {
					t.Fatalf("wrong remote descriptor: %v", err)
				}
				return
			}
			if err := f.publisher.CompleteCheckpoint(ctx, f.ref, f.manifest); err != nil {
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
			if err := f.publisher.RegisterCheckpoint(ctx, ref, f.manifest); !errors.Is(err, ErrDenied) {
				t.Fatalf("foreign registration: %v", err)
			}
			if err := f.publisher.CompleteCheckpoint(ctx, ref, f.manifest); !errors.Is(err, ErrDenied) {
				t.Fatalf("foreign completion: %v", err)
			}
			wrong := f.manifest
			wrong.ComputerID = uuid.NewV7()
			wrong.LeaseEpoch++
			wrong.ControlVersion++
			if err := f.publisher.CompleteCheckpoint(ctx, ref, wrong); !errors.Is(err, ErrDenied) {
				t.Fatalf("foreign identity probing: %v", err)
			}
		})
	}
}

func TestCheckpointRuntimeReadyCannotReplayAfterAbort(t *testing.T) {
	f := newCheckpointStorageFixture(t)
	ctx := t.Context()
	if err := f.publisher.RegisterCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	f.upload(t)
	if err := f.publish(t, f.save, f.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	if err := f.publisher.CompleteCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	p, _ := sourceAbortForCheckpoint(t, f.f, f.manifest.CheckpointID)
	if err := ValidateComputerSourceAbort(ctx, f.f.pool, f.ref.Host, f.f.env, p, abortReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"aborting", "consumed"} {
		if state == "consumed" {
			if err := CommitComputerSourceAbort(ctx, f.f.pool, f.ref.Host, f.f.env, p, abortReceipt(p, true, false)); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.publisher.RegisterCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, ErrNotReady) {
			t.Fatalf("%s registration replay: %v", state, err)
		}
		if err := f.publisher.CompleteCheckpoint(ctx, f.ref, f.manifest); !errors.Is(err, ErrNotReady) {
			t.Fatalf("%s ready replay: %v", state, err)
		}
	}
}

func TestCheckpointRuntimeAbortDuringRemoteVerification(t *testing.T) {
	f := newCheckpointStorageFixture(t)
	ctx := t.Context()
	if err := f.publisher.RegisterCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	f.upload(t)
	if err := f.publish(t, f.save, f.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	p, _ := sourceAbortForCheckpoint(t, f.f, f.manifest.CheckpointID)
	entered, release := make(chan struct{}), make(chan struct{})
	first := true
	blocked, _ := NewSavePublisher(f.f.pool, saveStatFunc(func(ctx context.Context, d string) (cas.Object, error) {
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
	if err := ValidateComputerSourceAbort(ctx, f.f.pool, f.ref.Host, f.f.env, p, abortReceipt(p, false, false)); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrNotReady) {
		t.Fatalf("aborted candidate became ready: %v", err)
	}
	var state string
	var ready bool
	if err := f.f.pool.QueryRow(ctx, `SELECT status,ready_at IS NOT NULL FROM computer_checkpoints WHERE id=$1`, f.manifest.CheckpointID).Scan(&state, &ready); err != nil || state != "aborting" || ready {
		t.Fatalf("state=%s ready=%v %v", state, ready, err)
	}
}
