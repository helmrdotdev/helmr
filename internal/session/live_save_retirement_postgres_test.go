package session_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

type liveSavePublication struct {
	storage *saveStorageFixture
	ref     computer.SavePublication
	uploads atomic.Int64
}

func (p *liveSavePublication) RegisterBatch(ctx context.Context, i []blockformat.ObjectInspection) error {
	return p.storage.publisher.RegisterBatch(ctx, p.ref, i)
}
func (p *liveSavePublication) CertifyBatch(ctx context.Context, i []blockformat.ObjectInspection) error {
	return p.storage.publisher.CertifyBatch(ctx, p.ref, i)
}
func (p *liveSavePublication) Reuse(ctx context.Context, i blockformat.ObjectInspection) error {
	if err := p.RegisterBatch(ctx, []blockformat.ObjectInspection{i}); err != nil {
		return err
	}
	return p.CertifyBatch(ctx, []blockformat.ObjectInspection{i})
}
func (p *liveSavePublication) Upload(ctx context.Context, d cas.Descriptor, f *os.File) (cas.Object, error) {
	p.uploads.Add(1)
	return p.storage.remote.Put(ctx, d.MediaType, io.NewSectionReader(f, 0, d.SizeBytes))
}

type liveSaveReclamation struct {
	store   *cas.File
	deleted atomic.Int64
}

func (*liveSaveReclamation) RetiredUploads(context.Context, string) ([]string, error) {
	return nil, nil
}
func (*liveSaveReclamation) ReclaimUpload(context.Context, string, string) error {
	return errors.New("file CAS has no multipart uploads")
}
func (s *liveSaveReclamation) ReclaimVersions(ctx context.Context, digest string) error {
	err := s.store.Delete(ctx, digest)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err == nil {
		s.deleted.Add(1)
	}
	return err
}

func TestLiveSaveRetirementExactIdentityAndMonotonicRetry(t *testing.T) {
	f := newFixture(t)
	storage := newSaveStorageFixture(t, f)
	var saves []*computer.SaveRequest
	var roots []disk.VersionRoot
	for i := range 3 {
		save, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, true, nil)
		if err != nil || save == nil {
			t.Fatalf("admit: %v", err)
		}
		root, _ := storage.cut(t, byte(i+10))
		f.capture(t, *save, mustRootDigest(t, root))
		if err = storage.publish(t, save.ID, root); err != nil {
			t.Fatal(err)
		}
		saves = append(saves, save)
		roots = append(roots, root)
	}
	advance := func(identity computer.LeaseIdentity, save uuid.UUID, root disk.VersionRoot) error {
		_, err := computer.NextSave(t.Context(), f.pool, *f.host(), identity, true, false, &computer.SaveRetirement{SaveID: save, Root: root})
		return err
	}
	if err := advance(saveIdentity(f), saves[1].ID, roots[1]); err != nil {
		t.Fatal(err)
	}
	// Model a committed request whose response was lost, then an older retry.
	for _, i := range []int{1, 0, 1} {
		if err := advance(saveIdentity(f), saves[i].ID, roots[i]); err != nil {
			t.Fatal(err)
		}
	}
	var floor int64
	if err := f.pool.QueryRow(t.Context(), `SELECT save_retirement_seq FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, f.env, f.computer).Scan(&floor); err != nil || floor != saves[1].Sequence {
		t.Fatalf("floor %d %v", floor, err)
	}
	if err := advance(saveIdentity(f), saves[1].ID, roots[0]); !errors.Is(err, computer.ErrConflict) {
		t.Fatalf("wrong exact save root accepted: %v", err)
	}
	if err := advance(saveIdentity(f), uuid.NewV7(), roots[1]); !errors.Is(err, computer.ErrConflict) {
		t.Fatalf("unknown save accepted: %v", err)
	}
	wrong := saveIdentity(f)
	wrong.Epoch++
	if err := advance(wrong, saves[1].ID, roots[1]); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("wrong lease accepted: %v", err)
	}
	wrong = saveIdentity(f)
	wrong.ComputerID = uuid.NewV7()
	if err := advance(wrong, saves[1].ID, roots[1]); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("wrong Computer accepted: %v", err)
	}
	if err := computer.ReconcileDiskRetention(t.Context(), f.pool); err != nil {
		t.Fatal(err)
	}
	var correct bool
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT root_id IS NULL FROM computer_saves WHERE id=$1) AND (SELECT root_id IS NOT NULL FROM computer_saves WHERE id=$2) AND (SELECT root_id IS NOT NULL FROM computer_saves WHERE id=$3)`, saves[0].ID, saves[1].ID, saves[2].ID).Scan(&correct); err != nil || !correct {
		t.Fatalf("retired current or newer floor: %v %v", correct, err)
	}
	// The old receipt remains valid for lost-ACK reconciliation after retirement.
	if err := advance(saveIdentity(f), saves[0].ID, roots[0]); err != nil {
		t.Fatal(err)
	}
}
func mustRootDigest(t *testing.T, root disk.VersionRoot) string {
	t.Helper()
	s, err := root.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLiveSaveRetirementSustainedOverwriteSweepsPhysicalCAS(t *testing.T) {
	f := newFixture(t)
	storage := newSizedSaveStorageFixture(t, f, 2<<20)
	base, err := disk.NewVersionRoot(storage.base, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	cfg := disk.LocalVersionConfig{Directory: filepath.Join(t.TempDir(), "live"), Base: base, BaseSource: storage.remote, Scope: storage.writer.Scope, ActiveKey: storage.writer.ActiveKey, Keys: storage.writer.Keys, DirtyBlocks: 512, StagedBytes: 64 << 20, PackLimit: blockformat.MinPackLimit}
	local, err := disk.CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	reclaims := &liveSaveReclamation{store: storage.remote}
	reclaimer, err := computer.NewCASReclaimer(f.pool, reclaims, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	shared := bytes.Repeat([]byte{42}, 4096)
	if _, err = local.WriteAt(t.Context(), shared, 1536<<10); err != nil {
		t.Fatal(err)
	}
	// Concurrent readers keep serving a shared range through real disk traversal
	// while each overwritten range gets a new authenticated save.
	readerCtx, stopReader := context.WithCancel(t.Context())
	readerDone := make(chan error, 1)
	var reads atomic.Int64
	go func() {
		b := make([]byte, 4096)
		for readerCtx.Err() == nil {
			if _, err := local.ReadAt(readerCtx, b, 1536<<10); err != nil {
				if readerCtx.Err() != nil {
					break
				}
				readerDone <- err
				return
			}
			if !bytes.Equal(b, shared) {
				readerDone <- errors.New("shared bytes changed")
				return
			}
			reads.Add(1)
		}
		readerDone <- nil
	}()
	defer func() {
		stopReader()
		if err := <-readerDone; err != nil {
			t.Error(err)
		}
	}()
	var lastRoot disk.VersionRoot
	var plateauBytes int64
	var plateauObjects int
	for i := range 24 {
		save, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, true, nil)
		if err != nil || save == nil {
			t.Fatalf("admit: %v", err)
		}
		want := bytes.Repeat([]byte{byte(i + 1)}, 1<<20)
		if _, err = local.WriteAt(t.Context(), want, 0); err != nil {
			t.Fatal(err)
		}
		capture, err := local.Capture(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		root := capture.Root()
		lastRoot = root
		if err = storage.publisher.Capture(t.Context(), storage.ref(save.ID), root, "live overwrite cut"); err != nil {
			t.Fatal(err)
		}
		pub := &liveSavePublication{storage: storage, ref: storage.ref(save.ID)}
		if err = capture.Publish(t.Context(), pub); err != nil {
			t.Fatal(err)
		}
		if err = storage.publisher.Publish(t.Context(), pub.ref, root, "live overwrite publication"); err != nil {
			t.Fatal(err)
		}
		if err = capture.Adopt(t.Context()); err != nil {
			t.Fatal(err)
		}
		capture.Release()
		// Brief readers may win individual attempts; bounded fixture retries measure
		// healthy progress only, never an unconditional production deadline.
		ready := false
		for range 10000 {
			runtime.Gosched()
			if capture.ReadyForRetirement() {
				ready = true
				break
			}
		}
		if !ready {
			t.Fatal("healthy reader fixture failed to provide a retirement opportunity")
		}
		if _, err = computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, false, &computer.SaveRetirement{SaveID: save.ID, Root: root}); err != nil {
			t.Fatal(err)
		}
		// Drain parent-first graph levels and the real CAS lifecycle. Physical bytes
		// are removed through CASReclaimer, not simulated by counting root rows.
		for range 8 {
			if err = computer.ReconcileDiskRetention(t.Context(), f.pool); err != nil {
				t.Fatal(err)
			}
			if err = reclaimer.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		objects, total := liveRemoteSize(t, storage)
		if i == 4 {
			plateauObjects, plateauBytes = objects, total
		}
		// Root pack serialization can vary by a few bytes; allow one metadata
		// block while requiring a fixed physical object count.
		if i > 4 && (objects > plateauObjects || total > plateauBytes+4096) {
			t.Fatalf("physical history grew at save %d: objects %d -> %d bytes %d -> %d", i, plateauObjects, objects, plateauBytes, total)
		}
		if i == 23 {
			t.Logf("24 overwrites: remote objects=%d bytes=%d physical deletions=%d concurrent reads=%d", objects, total, reclaims.deleted.Load(), reads.Load())
		}
	}
	if reclaims.deleted.Load() == 0 || reads.Load() == 0 {
		t.Fatal("fixture did not exercise physical deletion and concurrent reads")
	}
	var pending int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_saves WHERE environment_id=$1 AND root_id IS NOT NULL`, f.env).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("save backlog did not drain: %d %v", pending, err)
	}
	var graphObjects, failedSweeps int
	if err = f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM disk_objects WHERE environment_id=$1),(SELECT count(*) FROM cas_blobs WHERE retired_at IS NOT NULL AND last_reclaim_error IS NOT NULL)`, f.env).Scan(&graphObjects, &failedSweeps); err != nil || graphObjects != plateauObjects || failedSweeps != 0 {
		t.Fatalf("graph/sweep backlog did not drain: objects=%d expected=%d failures=%d err=%v", graphObjects, plateauObjects, failedSweeps, err)
	}
	// Drop pins from the preceding published root to exercise surviving-root
	// admission directly, including uncached reuse of its shared dependencies.
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM disk_object_pins WHERE environment_id=$1`, f.env)
	next, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, true, nil)
	if err != nil || next == nil {
		t.Fatal(err)
	}
	if err = storage.publisher.Capture(t.Context(), storage.ref(next.ID), lastRoot, "same cached root"); err != nil {
		t.Fatal(err)
	}
	capture, err := local.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Release()
	cached := &liveSavePublication{storage: storage, ref: storage.ref(next.ID)}
	if err = capture.Publish(t.Context(), cached); err != nil {
		t.Fatal(err)
	}
	if cached.uploads.Load() != 0 {
		t.Fatal("cached root issued redundant PUT after old pins removed")
	}
	locator, _ := lastRoot.Locator(lastRoot.LogicalBytes)
	inspected, err := blockformat.InspectPack(t.Context(), storage.remote, cfg.Scope, cfg.Keys, locator.Pack)
	if err != nil {
		t.Fatal(err)
	}
	// Registration validates the root's children through retained published-root
	// closure after every original save object pin has been removed.
	if err = cached.Reuse(t.Context(), blockformat.ObjectInspection{Pack: &inspected}); err != nil {
		t.Fatal(err)
	}
	if err = storage.publisher.Publish(t.Context(), cached.ref, lastRoot, "surviving root publication"); err != nil {
		t.Fatal(err)
	}
	restored, err := disk.OpenVersion(t.Context(), storage.remote, cfg.Scope, cfg.Keys, lastRoot, cfg.Base.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	b, err := restored.ReadRange(t.Context(), 0, 1<<20)
	if err != nil || !bytes.Equal(b, bytes.Repeat([]byte{24}, len(b))) {
		t.Fatalf("restored overwrite bytes: %v", err)
	}
	b, err = restored.ReadRange(t.Context(), 1536<<10, 4096)
	if err != nil || !bytes.Equal(b, shared) {
		t.Fatalf("restored shared bytes: %v", err)
	}
}

func liveRemoteSize(t *testing.T, s *saveStorageFixture) (int, int64) {
	t.Helper()
	rows, err := s.f.pool.Query(t.Context(), `SELECT digest FROM cas_blobs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	var total int64
	for rows.Next() {
		var digest string
		if err = rows.Scan(&digest); err != nil {
			t.Fatal(err)
		}
		object, err := s.remote.Stat(t.Context(), digest)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
		total += object.SizeBytes
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return count, total
}

func TestLiveSaveRetirementPreservesCheckpointRestoreHeadAndKeys(t *testing.T) {
	f := newFixture(t)
	storage := newSaveStorageFixture(t, f)
	var saves []*computer.SaveRequest
	var roots []disk.VersionRoot
	for i := range 3 {
		save, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, true, nil)
		if err != nil || save == nil {
			t.Fatal(err)
		}
		root, _ := storage.cut(t, byte(i+20))
		f.capture(t, *save, mustRootDigest(t, root))
		if err = storage.publish(t, save.ID, root); err != nil {
			t.Fatal(err)
		}
		saves = append(saves, save)
		roots = append(roots, root)
	}
	if _, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, false, &computer.SaveRetirement{SaveID: saves[2].ID, Root: roots[2]}); err != nil {
		t.Fatal(err)
	}
	// Attach each independently durable owner directly to isolate retention's
	// exclusion predicates; coherent guest capture/restore has separate tests.
	checkpoint := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO checkpoints(environment_id,id,computer_id,source_lease_epoch,control_version,disk_save_id,status,capture_request,capture_expires_at,capture_digest) VALUES($1,$2,$3,1,1,$4,'capturing',decode('01','hex'),clock_timestamp()+interval '1 hour',decode(repeat('01',32),'hex'))`, f.env, checkpoint, f.computer, saves[0].ID)
	assertProtected := func() {
		t.Helper()
		for range 4 {
			if err := computer.ReconcileDiskRetention(t.Context(), f.pool); err != nil {
				t.Fatal(err)
			}
		}
		var valid bool
		if err := f.pool.QueryRow(t.Context(), `SELECT s.root_id IS NOT NULL AND l.disk_released_at IS NULL AND l.base_root_id IS NOT NULL AND base.id IS NOT NULL AND k.available AND (SELECT count(*) FROM disk_encryption_keys WHERE environment_id=$1 AND available)=2 FROM computer_saves s JOIN computer_leases l ON l.environment_id=s.environment_id AND l.computer_id=s.computer_id AND l.epoch=1 JOIN disk_roots base ON base.environment_id=l.environment_id AND base.id=l.base_root_id JOIN disk_encryption_keys k ON k.environment_id=l.environment_id AND k.id=l.write_key_id WHERE s.environment_id=$1 AND s.id=$2`, f.env, saves[0].ID).Scan(&valid); err != nil || !valid {
			t.Fatalf("protected save/base/write keys lost: %v %v", valid, err)
		}
	}
	assertProtected()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET restored_from_save_id=$2 WHERE environment_id=$1 AND epoch=1`, f.env, saves[0].ID)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE checkpoints SET status='lost',capture_request=NULL,terminal_evidence='fixture closure' WHERE environment_id=$1 AND id=$2`, f.env, checkpoint)
	assertProtected()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET recovery_save_id=$2 WHERE environment_id=$1`, f.env, saves[0].ID)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET restored_from_save_id=NULL WHERE environment_id=$1 AND epoch=1`, f.env)
	assertProtected()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET recovery_save_id=$2 WHERE environment_id=$1`, f.env, saves[2].ID)
	if err := computer.ReconcileDiskRetention(t.Context(), f.pool); err != nil {
		t.Fatal(err)
	}
	var retired bool
	if err := f.pool.QueryRow(t.Context(), `SELECT root_id IS NULL AND payload_retired_at IS NOT NULL FROM computer_saves WHERE id=$1`, saves[0].ID).Scan(&retired); err != nil || !retired {
		t.Fatalf("released custody prevented progress: %v %v", retired, err)
	}
}

func TestLiveSaveRetirementWaitsForOldestUnresolvedReceipt(t *testing.T) {
	f := newFixture(t)
	storage := newSaveStorageFixture(t, f)
	var saves []*computer.SaveRequest
	var roots []disk.VersionRoot
	for i := range 4 {
		save, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, true, nil)
		if err != nil || save == nil {
			t.Fatal(err)
		}
		root, _ := storage.cut(t, byte(i+30))
		f.capture(t, *save, mustRootDigest(t, root))
		if err = storage.publish(t, save.ID, root); err != nil {
			t.Fatal(err)
		}
		saves = append(saves, save)
		roots = append(roots, root)
	}
	if _, err := computer.NextSave(t.Context(), f.pool, *f.host(), saveIdentity(f), true, false, &computer.SaveRetirement{SaveID: saves[3].ID, Root: roots[3]}); err != nil {
		t.Fatal(err)
	}
	// An earlier captured publication can remain unresolved while later captures
	// have receipts. Preserve all history from that oldest unresolved sequence.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_saves SET status='captured',publication_evidence=NULL,root_id=NULL WHERE id=$1`, saves[1].ID)
	if err := computer.ReconcileDiskRetention(t.Context(), f.pool); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT root_id IS NULL FROM computer_saves WHERE id=$1) AND (SELECT root_id IS NOT NULL FROM computer_saves WHERE id=$3) AND EXISTS(SELECT 1 FROM disk_object_pins WHERE save_id=$2)`, saves[0].ID, saves[1].ID, saves[2].ID).Scan(&valid); err != nil || !valid {
		t.Fatalf("oldest unresolved protection: %v %v", valid, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_saves SET status='superseded',covered_by_save_id=$2 WHERE id=$1`, saves[1].ID, saves[3].ID)
	if err := computer.ReconcileDiskRetention(t.Context(), f.pool); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT root_id IS NULL FROM computer_saves WHERE id=$1) AND NOT EXISTS(SELECT 1 FROM disk_object_pins WHERE save_id=$2)`, saves[2].ID, saves[1].ID).Scan(&valid); err != nil || !valid {
		t.Fatalf("superseded receipt backlog did not drain: %v %v", valid, err)
	}
}
