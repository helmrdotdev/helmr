package computer_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// residentInstance is a running Run lease's Instance whose write key is
// pinned, with the principal of its worker host.
func residentInstance(t *testing.T) (runtest.Fixture, runtest.RunLease, workergroup.HostPrincipal, db.ComputerInstance) {
	t.Helper()
	f := runtest.New(t)
	member := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	var instance db.ComputerInstance
	var err error
	var instanceID uuid.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, member.LeaseID).Scan(&instanceID); err != nil {
		t.Fatal(err)
	}
	key := pgvalue.NewUUIDv7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) SELECT $2,environment_id,computer_id,'fixture',decode('01','hex') FROM computer_instances WHERE id=$1`, instanceID, key)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET write_key_id=$2 WHERE id=$1`, instanceID, key)
	if instance, err = db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{ID: pgvalue.UUID(instanceID), EnvironmentID: pgvalue.UUID(f.EnvironmentID)}); err != nil {
		t.Fatal(err)
	}
	principal := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	return f, member, principal, instance
}

// registerEmptyCapture finishes the resident process, begins the Instance's
// capture through the ordinary admission and registers its manifest.
func registerEmptyCapture(t *testing.T, f runtest.Fixture, member runtest.RunLease, instance db.ComputerInstance) (db.ComputerCheckpoint, computer.CheckpointRef) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled',process_reconciled_at=now() WHERE id=$1`, member.LeaseID)
	instance, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{ID: instance.ID, EnvironmentID: instance.EnvironmentID})
	if err != nil {
		t.Fatal(err)
	}
	var cp db.ComputerCheckpoint
	if err = db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
		var err error
		cp, err = computer.BeginCapture(t.Context(), tx, computer.Capture{InstanceID: pgvalue.MustUUIDValue(instance.ID), EnvironmentID: pgvalue.MustUUIDValue(instance.EnvironmentID), WriterGeneration: instance.WriterGeneration, MembershipRevision: instance.MembershipRevision, DesiredVersion: instance.DesiredVersion, CheckpointID: uuid.NewV7()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ref, manifest := computertest.CaptureRequest(t, f, cp)
	if _, err = computer.RegisterCheckpoint(t.Context(), f.Pool, ref, manifest); err != nil {
		t.Fatal(err)
	}
	return cp, ref
}

// Capture fences the host epoch and status, never claim versions: a claim
// bump or a drain does not stop a checkpoint from recording its objects, and
// a new epoch or a lost host does.
func TestCheckpointObjectsFenceEpochAndStatusNotClaims(t *testing.T) {
	for _, transition := range hostTransitions {
		t.Run(transition.name, func(t *testing.T) {
			f, member, _, instance := residentInstance(t)
			_, ref := registerEmptyCapture(t, f, member, instance)
			store, err := cas.NewFile(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			content := bytes.Repeat([]byte{7}, 64)
			if _, err = store.Put(t.Context(), "application/octet-stream", bytes.NewReader(content)); err != nil {
				t.Fatal(err)
			}
			publisher, err := computer.NewPublisher(f.Pool, store)
			if err != nil {
				t.Fatal(err)
			}
			inspection := blockformat.ObjectInspection{Segment: &blockformat.Ref{Digest: sha256.Sum256(content), Key: pgvalue.UUIDString(instance.WriteKeyID), Kind: blockformat.SegmentKind, Count: 1, Size: 64}}
			if err = transition.apply(t.Context(), f); err != nil {
				t.Fatal(err)
			}
			if transition.rejected {
				if err = publisher.RegisterCheckpointObject(t.Context(), ref, inspection); !errors.Is(err, computer.ErrAuthorityChanged) {
					t.Fatalf("registration after %s: %v", transition.name, err)
				}
				return
			}
			if err = publisher.RegisterCheckpointObject(t.Context(), ref, inspection); err != nil {
				t.Fatalf("registration after %s: %v", transition.name, err)
			}
			if err = publisher.CertifyCheckpointObject(t.Context(), ref, inspection); err != nil {
				t.Fatalf("certification after %s: %v", transition.name, err)
			}
			if err = publisher.ReuseCheckpointObject(t.Context(), ref, inspection); err != nil {
				t.Fatalf("reuse after %s: %v", transition.name, err)
			}
			var certified bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT certified FROM computer_objects WHERE digest=$1`, "sha256:"+hex.EncodeToString(inspection.Segment.Digest[:])).Scan(&certified); err != nil || !certified {
				t.Fatalf("checkpoint object certified=%v err=%v", certified, err)
			}
		})
	}
}

type retiredBlobStore struct {
	t     *testing.T
	q     db.Querier
	calls int
}

func (s *retiredBlobStore) RetiredUploads(context.Context, string) ([]string, error) {
	return nil, nil
}

func (s *retiredBlobStore) ReclaimUpload(context.Context, string, string) error {
	return errors.New("unexpected multipart upload")
}

func (s *retiredBlobStore) ReclaimVersions(ctx context.Context, digest string) error {
	s.calls++
	// The remote side effect sees a committed irreversible retirement fence.
	if _, err := s.q.UpsertCasObject(ctx, db.UpsertCasObjectParams{OrgID: pgvalue.NewUUIDv7(), Digest: digest, SizeBytes: 1, MediaType: "application/octet-stream"}); err == nil {
		s.t.Fatal("physical deletion preceded retirement commit")
	}
	return nil
}

func TestAbandonedSaveRetainsObjectsForLaterCheckpoint(t *testing.T) {
	f, member, worker, instance := residentInstance(t)
	objects, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := computer.NewPublisher(f.Pool, objects)
	if err != nil {
		t.Fatal(err)
	}
	save := computer.SaveRef{EnvironmentID: f.EnvironmentID, InstanceID: pgvalue.MustUUIDValue(instance.ID), WriterGeneration: instance.WriterGeneration, SaveID: uuid.NewV7(), Sequence: 1}
	if _, err = publisher.BeginSave(t.Context(), worker, save); err != nil {
		t.Fatal(err)
	}
	inspection := blockformat.ObjectInspection{Segment: &blockformat.Ref{Digest: sha256.Sum256([]byte("abandoned save candidate")), Key: pgvalue.UUIDString(instance.WriteKeyID), Kind: blockformat.SegmentKind, Count: 1, Size: 64}}
	if err = publisher.RegisterSaveObject(t.Context(), worker, save, inspection); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = publisher.AbandonSave(t.Context(), worker, save); err != nil {
			t.Fatal(err)
		}
	}
	store := &retiredBlobStore{t: t, q: db.New(f.Pool)}
	collector, err := computer.NewRetention(f.Pool, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err = collector.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if store.calls != 0 {
		t.Fatal("abandoned save ciphertext was retired while its VM can reuse it")
	}
	// Finish the resident process, then use the ordinary whole-instance capture
	// admission and publication owners for the same ciphertext.
	cp, ref := registerEmptyCapture(t, f, member, instance)
	if err = publisher.RegisterCheckpointObject(t.Context(), ref, inspection); err != nil {
		t.Fatalf("later checkpoint could not register the retained bytes: %v", err)
	}
	// Logical closure alone cannot release either publication's retention.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,observed_state='failed',terminal_at=clock_timestamp(),terminal_reason_code='fixture',admission_state='closed',mount_state='lost' WHERE id=$1`, instance.ID)
	if err = collector.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if store.calls != 0 {
		t.Fatal("logical closure released live ciphertext")
	}
	// The fixture supplies physical exclusion; the production collector then owns
	// pin release and immutable-byte retirement.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_checkpoints SET status='invalid',invalidated_at=clock_timestamp(),invalidation_reason_code='fixture' WHERE id=$1`, cp.ID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET reclaimed_at=clock_timestamp(),reclaim_evidence='{"proof":"fixture"}' WHERE id=$1`, instance.ID)
	if err = collector.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	var retired bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT retired_at IS NOT NULL AND NOT EXISTS(SELECT 1 FROM computer_object_pins WHERE digest=$1) FROM cas_blobs WHERE digest=$1`, "sha256:"+hex.EncodeToString(inspection.Segment.Digest[:])).Scan(&retired); err != nil || !retired {
		t.Fatalf("reclaimed candidate still retained: %v %v", retired, err)
	}
}

type unavailableObjects struct{}

func (unavailableObjects) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{}, errors.New("object storage timed out")
}

// Checkpoint certification reports unavailable object storage as
// ErrStorageUnavailable after verifying the registration.
func TestCheckpointObjectCertificationReportsUnavailableStorage(t *testing.T) {
	f, member, _, instance := residentInstance(t)
	_, ref := registerEmptyCapture(t, f, member, instance)
	publisher, err := computer.NewPublisher(f.Pool, unavailableObjects{})
	if err != nil {
		t.Fatal(err)
	}
	inspection := blockformat.ObjectInspection{Segment: &blockformat.Ref{Digest: sha256.Sum256([]byte("unavailable checkpoint object")), Key: pgvalue.UUIDString(instance.WriteKeyID), Kind: blockformat.SegmentKind, Count: 1, Size: 64}}
	if err = publisher.RegisterCheckpointObject(t.Context(), ref, inspection); err != nil {
		t.Fatal(err)
	}
	if err = publisher.CertifyCheckpointObject(t.Context(), ref, inspection); !errors.Is(err, computer.ErrStorageUnavailable) {
		t.Fatalf("certification with unavailable storage: %v", err)
	}
}

// A checkpoint object whose child node differs from the certified child's
// inspection is an object conflict, not an internal failure.
func TestCheckpointObjectWrongChildNodeIsConflict(t *testing.T) {
	f, member, _, instance := residentInstance(t)
	_, ref := registerEmptyCapture(t, f, member, instance)
	key := pgvalue.UUIDString(instance.WriteKeyID)
	shape := blockformat.Root{Capacity: instance.ReservedGuestEphemeralDiskBytes, Fanout: 64, Level: 1}
	child := blockformat.Locator{Pack: blockformat.PackRef{Digest: sha256.Sum256([]byte("checkpoint child node")), Size: 64, Rank: 1}, Page: blockformat.Ref{Key: key, Kind: blockformat.NodeKind, Count: 1, Size: 32}, Offset: 8}
	childInspection := blockformat.ObjectInspection{Pack: &blockformat.PackInspection{Pages: []blockformat.PageInspection{{Locator: child, Shape: shape}}, Keys: []string{key}}}
	publisher, err := computer.NewPublisher(f.Pool, computertest.Objects{{Digest: "sha256:" + hex.EncodeToString(child.Pack.Digest[:]), SizeBytes: 64, MediaType: "application/octet-stream"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = publisher.RegisterCheckpointObject(t.Context(), ref, childInspection); err != nil {
		t.Fatal(err)
	}
	if err = publisher.CertifyCheckpointObject(t.Context(), ref, childInspection); err != nil {
		t.Fatal(err)
	}
	parent := blockformat.Locator{Pack: blockformat.PackRef{Digest: sha256.Sum256([]byte("checkpoint parent node")), Size: 64, Rank: 2}, Page: blockformat.Ref{Key: key, Kind: blockformat.NodeKind, Count: 1, Size: 32}, Offset: 8}
	wrongPosition := blockformat.NodeReference{Locator: child, Shape: shape, Start: 1}
	parentInspection := blockformat.ObjectInspection{Pack: &blockformat.PackInspection{Pages: []blockformat.PageInspection{{Locator: parent, Shape: shape, Level: 1, Children: []blockformat.NodeReference{wrongPosition}}}, Keys: []string{key}}}
	var conflict computer.ObjectConflictError
	if err = publisher.RegisterCheckpointObject(t.Context(), ref, parentInspection); !errors.As(err, &conflict) || !strings.Contains(err.Error(), "child node differs") {
		t.Fatalf("wrong child node = %v", err)
	}
}
