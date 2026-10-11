package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/jackc/pgx/v5"
)

type saveStorageFixture struct {
	f             fixture
	local, remote *cas.File
	writer        blockformat.Writer
	base          blockformat.Locator
	publisher     *SavePublisher
}

// The initial-image producer is outside this save protocol. Bootstrap its exact
// authenticated empty root here, then exercise real registration/certification
// and publication for every new cut against PostgreSQL and a separate file CAS.
func newSaveStorageFixture(t *testing.T, f fixture) *saveStorageFixture {
	t.Helper()
	local, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	remote, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keyID, writeKeyID := uuid.NewV7(), uuid.NewV7()
	writer := blockformat.Writer{Source: local, Sink: local, Scope: f.env.String(), ActiveKey: keyID.String(), Keys: map[string][]byte{keyID.String(): bytes.Repeat([]byte{7}, 32)}, PackLimit: blockformat.MinPackLimit}
	base, err := writer.Empty(t.Context(), 1<<20, 64)
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := blockformat.InspectPack(t.Context(), local, writer.Scope, writer.Keys, base.Pack)
	if err != nil {
		t.Fatal(err)
	}
	object, err := describeSaveObject(blockformat.ObjectInspection{Pack: &inspected})
	if err != nil {
		t.Fatal(err)
	}
	root, err := disk.NewVersionRoot(base, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	rootJSON, _ := json.Marshal(root)
	rootIdentity, _ := root.Digest()
	rootID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `
      INSERT INTO computer_data_keys(id,environment_id,writer_computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,NULL,'test',decode('01','hex')),($11,$2,$3,'test',decode('02','hex'));
      INSERT INTO cas_blobs(digest,size_bytes) VALUES($4,$5);
      INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$4,$5,'application/octet-stream' FROM environments WHERE id=$2;
      INSERT INTO computer_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at) SELECT id,$4,org_id,project_id,$5,'application/octet-stream','root',$6,$7,clock_timestamp() FROM environments WHERE id=$2;
      INSERT INTO computer_object_keys(environment_id,digest,key_id,is_direct) VALUES($2,$4,$1,true);
      INSERT INTO computer_disk_roots(environment_id,id,locator) VALUES($2,$8,$9);
      UPDATE computers SET initial_root_id=$8,initial_root_digest=decode(substring($10::text from 8),'hex') WHERE environment_id=$2 AND id=$3;
    `, pgx.QueryExecModeSimpleProtocol, keyID, f.env, f.computer, object.digest, object.size, object.rank, string(object.encoded), rootID, string(rootJSON), rootIdentity, writeKeyID)
	if actual, err := BindComputerLeaseDisk(t.Context(), f.pool, *f.host(), f.env, f.computer, 1, writeKeyID); err != nil || actual != root {
		t.Fatalf("lease disk binding: %v", err)
	}
	writer.ActiveKey = writeKeyID.String()
	writer.Keys[writeKeyID.String()] = bytes.Repeat([]byte{6}, 32)
	p, err := NewSavePublisher(f.pool, remote)
	if err != nil {
		t.Fatal(err)
	}
	s := &saveStorageFixture{f: f, local: local, remote: remote, writer: writer, base: base, publisher: p}
	s.uploadObject(t, object)
	return s
}

func (s *saveStorageFixture) ref(save uuid.UUID) SavePublication {
	return SavePublication{EnvironmentID: s.f.env, SaveID: save, LeaseEpoch: 1, Host: *s.f.host()}
}

func (s *saveStorageFixture) cut(t *testing.T, value byte) (disk.VersionRoot, string) {
	t.Helper()
	root, err := s.writer.Capture(t.Context(), s.base, 1<<20, map[uint64][]byte{0: bytes.Repeat([]byte{value}, 4096), 100: bytes.Repeat([]byte{value}, 4096)})
	if err != nil {
		t.Fatal(err)
	}
	result, err := disk.NewVersionRoot(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := result.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return result, identity
}

func (s *saveStorageFixture) uploadObject(t *testing.T, object saveObject) {
	t.Helper()
	d := cas.Descriptor{Digest: object.digest, SizeBytes: object.size, MediaType: "application/octet-stream"}
	file, err := s.local.OpenImmutable(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err = cas.VerifyDescriptorFile(t.Context(), d, file); err != nil {
		t.Fatal(err)
	}
	if _, err = s.remote.Put(t.Context(), d.MediaType, io.NewSectionReader(file, 0, d.SizeBytes)); err != nil {
		t.Fatal(err)
	}
}

func (s *saveStorageFixture) inspect(t *testing.T, root disk.VersionRoot) []blockformat.ObjectInspection {
	t.Helper()
	locator, err := root.Locator(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	var result []blockformat.ObjectInspection
	seen := map[string]bool{}
	record := func(e blockformat.ObjectInspection) {
		object, err := describeSaveObject(e)
		if err != nil {
			t.Fatal(err)
		}
		if seen[object.digest] {
			return
		}
		seen[object.digest] = true
		result = append(result, e)
	}
	var visit func(blockformat.PackRef)
	visit = func(pack blockformat.PackRef) {
		e, err := blockformat.InspectPack(t.Context(), s.local, s.writer.Scope, s.writer.Keys, pack)
		if err != nil {
			t.Fatal(err)
		}
		for _, page := range e.Pages {
			for _, child := range page.Children {
				visit(child.Locator.Pack)
			}
			for _, child := range page.Segments {
				if err = blockformat.InspectSegment(t.Context(), s.local, s.writer.Scope, s.writer.Keys[child.Key], child); err != nil {
					t.Fatal(err)
				}
				record(blockformat.ObjectInspection{Segment: &child})
			}
		}
		record(blockformat.ObjectInspection{Pack: &e})
	}
	visit(locator.Pack)
	return result
}

func (s *saveStorageFixture) certify(t *testing.T, ref SavePublication, root disk.VersionRoot) []blockformat.ObjectInspection {
	t.Helper()
	inspections := s.inspect(t, root)
	for _, e := range inspections {
		object, err := describeSaveObject(e)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.publisher.Register(t.Context(), ref, e); err != nil {
			t.Fatal(err)
		}
		s.uploadObject(t, object)
		if err = s.publisher.Certify(t.Context(), ref, e); err != nil {
			t.Fatal(err)
		}
	}
	return inspections
}

func (s *saveStorageFixture) publish(t *testing.T, save uuid.UUID, root disk.VersionRoot) error {
	t.Helper()
	ref := s.ref(save)
	s.certify(t, ref, root)
	return s.publisher.Publish(t.Context(), ref, root, "verified complete retained disk graph")
}

func TestSavePublicationRetainsAuthenticatedGraph(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	a, save := f.finalize(t, "save")
	root, identity := s.cut(t, 3)
	f.capture(t, save, identity)
	if err := s.publish(t, save.ID, root); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, a.TurnID); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileSavePublication(t.Context(), f.pool, f.env, save.ID, identity); err != nil {
		t.Fatal(err)
	}
	var roots, pins, keys int
	err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM computer_saves WHERE id=$1 AND root_id IS NOT NULL),(SELECT count(*) FROM computer_object_pins WHERE save_id=$1),(SELECT count(*) FROM computer_object_keys WHERE digest=$2)`, save.ID, root.Pack.Digest).Scan(&roots, &pins, &keys)
	if err != nil || roots != 1 || pins < 3 || keys != 1 {
		t.Fatalf("retention roots=%d pins=%d keys=%d: %v", roots, pins, keys, err)
	}
	// Retained bytes and keys cannot be collected while the Save still owns them.
	if _, err = f.pool.Exec(t.Context(), `DELETE FROM computer_objects WHERE environment_id=$1 AND digest=$2`, f.env, root.Pack.Digest); err == nil {
		t.Fatal("collected retained root")
	}
	if _, err = f.pool.Exec(t.Context(), `UPDATE computer_data_keys SET retired_at=clock_timestamp(),wrapped_key=NULL WHERE id=$1`, s.writer.ActiveKey); err == nil {
		t.Fatal("retired retained key")
	}
	if _, err = f.pool.Exec(t.Context(), `UPDATE cas_blobs SET retired_at=clock_timestamp(),next_reclaim_at=clock_timestamp() WHERE digest=$1`, root.Pack.Digest); err == nil {
		t.Fatal("retired retained bytes")
	}
}

type saveStatFunc func(context.Context, string) (cas.Object, error)

func (f saveStatFunc) Stat(ctx context.Context, digest string) (cas.Object, error) {
	return f(ctx, digest)
}

func TestSavePublicationRetryRestartAndLostAcknowledgement(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	a, save := f.finalize(t, "one-processing-result")
	root, identity := s.cut(t, 9)
	f.capture(t, save, identity)
	ref := s.ref(save.ID)
	inspections := s.inspect(t, root)
	// Certify children, then lose access to storage while the root is pending.
	for _, e := range inspections[:len(inspections)-1] {
		if err := s.publisher.Register(t.Context(), ref, e); err != nil {
			t.Fatal(err)
		}
		object, _ := describeSaveObject(e)
		s.uploadObject(t, object)
		if err := s.publisher.Certify(t.Context(), ref, e); err != nil {
			t.Fatal(err)
		}
	}
	e := inspections[len(inspections)-1]
	if err := s.publisher.Register(t.Context(), ref, e); err != nil {
		t.Fatal(err)
	}
	object, _ := describeSaveObject(e)
	s.uploadObject(t, object)
	unavailable, _ := NewSavePublisher(f.pool, saveStatFunc(func(context.Context, string) (cas.Object, error) {
		return cas.Object{}, errors.New("temporarily unavailable")
	}))
	if err := unavailable.Certify(t.Context(), ref, e); !errors.Is(err, ErrSaveStorageUnavailable) {
		t.Fatalf("transient storage: %v", err)
	}
	if err := ReconcileSavePublication(t.Context(), f.pool, f.env, save.ID, identity); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unknown publication: %v", err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, a.TurnID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unpublished success: %v", err)
	}
	queued := f.enqueue(t, "accepted-during-storage-retry")
	if current, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil || current.TurnID != a.TurnID || current.Status != "finalizing" {
		t.Fatalf("dispatched through unsettled save: %v %v", current, err)
	}
	// The persistence service restarts, not the customer process or physical
	// writer. Exact local/remote bytes, the Save and unexpired authority survive.
	restarted, err := NewSavePublisher(f.pool, s.remote)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.Certify(t.Context(), ref, e); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Publish(t.Context(), ref, root, "same retained operation after restart"); err != nil {
		t.Fatal(err)
	}
	// Ignore the successful response and reconcile from a new service instance.
	if err = ReconcileSavePublication(t.Context(), f.pool, f.env, save.ID, identity); err != nil {
		t.Fatal(err)
	}
	if err = Complete(t.Context(), f.pool, f.env, f.session, a.TurnID); err != nil {
		t.Fatal(err)
	}
	if err = Complete(t.Context(), f.pool, f.env, f.session, a.TurnID); err != nil {
		t.Fatal(err)
	}
	next, err := Dispatch(t.Context(), f.pool, f.execution())
	if err != nil || next.TurnID != queued.TurnID {
		t.Fatalf("queued dispatch: %v %v", next, err)
	}
	var completedEvents, saves, holds int
	err = f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM session_events WHERE turn_id=$1 AND kind='turn.completed'),(SELECT count(*) FROM computer_saves WHERE turn_id=$1),(SELECT count(*) FROM session_holds WHERE session_id=$2)`, a.TurnID, f.session).Scan(&completedEvents, &saves, &holds)
	if err != nil || completedEvents != 1 || saves != 1 || holds != 0 {
		t.Fatalf("duplicate work or manual recovery: events=%d saves=%d holds=%d %v", completedEvents, saves, holds, err)
	}
}

func TestSavePublicationExpiryDoesNotAuthorizeSalvage(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncommitted", true: "committed-response-lost"}[committed], func(t *testing.T) {
			f := newFixture(t)
			s := newSaveStorageFixture(t, f)
			a, save := f.finalize(t, "save")
			root, identity := s.cut(t, 4)
			f.capture(t, save, identity)
			ref := s.ref(save.ID)
			inspections := s.certify(t, ref, root)
			if committed {
				if err := s.publisher.Publish(t.Context(), ref, root, "committed with valid writer"); err != nil {
					t.Fatal(err)
				}
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
			err := s.publisher.Publish(t.Context(), ref, root, "attempt after expiry")
			if committed {
				if err != nil {
					t.Fatal(err)
				}
				if err = ReconcileSavePublication(t.Context(), f.pool, f.env, save.ID, identity); err != nil {
					t.Fatal(err)
				}
				if err = Complete(t.Context(), f.pool, f.env, f.session, a.TurnID); err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, ErrDenied) {
					t.Fatalf("new publication after expiry: %v", err)
				}
				if err = ReconcileSavePublication(t.Context(), f.pool, f.env, save.ID, identity); !errors.Is(err, ErrNotReady) {
					t.Fatalf("salvaged uncommitted objects: %v", err)
				}
				if err = s.publisher.Register(t.Context(), ref, inspections[0]); !errors.Is(err, ErrDenied) {
					t.Fatalf("new object admission after expiry: %v", err)
				}
				if err = s.publisher.Certify(t.Context(), ref, inspections[0]); !errors.Is(err, ErrDenied) {
					t.Fatalf("new certification after expiry: %v", err)
				}
			}
			var retained int
			if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_object_pins WHERE save_id=$1`, save.ID).Scan(&retained); err != nil || retained != len(inspections) {
				t.Fatalf("uncertainty released pins: %d %v", retained, err)
			}
		})
	}
}

func TestSaveCertificationRechecksAuthorityAfterStorageIO(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	_, save := f.finalize(t, "save")
	root, identity := s.cut(t, 8)
	f.capture(t, save, identity)
	ref := s.ref(save.ID)
	e := s.inspect(t, root)[0]
	if err := s.publisher.Register(t.Context(), ref, e); err != nil {
		t.Fatal(err)
	}
	object, _ := describeSaveObject(e)
	s.uploadObject(t, object)
	entered, release := make(chan struct{}), make(chan struct{})
	p, _ := NewSavePublisher(f.pool, saveStatFunc(func(ctx context.Context, digest string) (cas.Object, error) {
		close(entered)
		<-release
		return s.remote.Stat(ctx, digest)
	}))
	result := make(chan error, 1)
	go func() { result <- p.Certify(t.Context(), ref, e) }()
	<-entered
	// This update would block if remote I/O held the Computer's transaction.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, updateErr := f.pool.Exec(ctx, `UPDATE computers SET next_save_seq=next_save_seq WHERE environment_id=$1 AND id=$2;
        UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND computer_id=$2`, pgx.QueryExecModeSimpleProtocol, f.env, f.computer)
	close(release)
	certifyErr := <-result
	if updateErr != nil {
		t.Fatal(updateErr)
	}
	if !errors.Is(certifyErr, ErrDenied) {
		t.Fatalf("expired certification: %v", certifyErr)
	}
	var certified bool
	if err := f.pool.QueryRow(t.Context(), `SELECT certified FROM computer_objects WHERE environment_id=$1 AND digest=$2`, f.env, object.digest).Scan(&certified); err != nil || certified {
		t.Fatalf("stale writer certified bytes: %v", err)
	}
}

func TestSavePublicationRejectsUncertifiedAndChangedGraph(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	_, save := f.finalize(t, "save")
	root, identity := s.cut(t, 5)
	f.capture(t, save, identity)
	ref := s.ref(save.ID)
	inspections := s.inspect(t, root)
	rootEvidence := inspections[len(inspections)-1]
	if err := s.publisher.Register(t.Context(), ref, rootEvidence); !errors.Is(err, ErrNotReady) {
		t.Fatalf("registered parent with uncertified children: %v", err)
	}
	var pins int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_object_pins WHERE save_id=$1`, save.ID).Scan(&pins); err != nil || pins != 0 {
		t.Fatalf("failed registration leaked pins: %d %v", pins, err)
	}
	for _, e := range inspections[:len(inspections)-1] {
		if err := s.publisher.Register(t.Context(), ref, e); err != nil {
			t.Fatal(err)
		}
		object, _ := describeSaveObject(e)
		s.uploadObject(t, object)
		if err := s.publisher.Certify(t.Context(), ref, e); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(rootEvidence)
	var changed blockformat.ObjectInspection
	if err := json.Unmarshal(raw, &changed); err != nil {
		t.Fatal(err)
	}
	if len(changed.Pack.Pages[0].Children) == 0 {
		t.Fatal("fixture needs child position")
	}
	changed.Pack.Pages[0].Children[0].Locator.Pack.Size++
	if err := s.publisher.Register(t.Context(), ref, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("child geometry mismatch misclassified: %v", err)
	}
	changed.Pack.Pages[0].Children[0].Locator.Pack.Size--
	changed.Pack.Pages[0].Children[0].Start++
	if err := s.publisher.Register(t.Context(), ref, changed); err == nil {
		t.Fatal("child's wrong position accepted")
	}
	if err := s.publisher.Register(t.Context(), ref, rootEvidence); err != nil {
		t.Fatal(err)
	}
	if err := s.publisher.Publish(t.Context(), ref, root, "root not uploaded"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("published uncertified root: %v", err)
	}
	object, _ := describeSaveObject(rootEvidence)
	s.uploadObject(t, object)
	wrongStore, _ := NewSavePublisher(f.pool, saveStatFunc(func(ctx context.Context, digest string) (cas.Object, error) {
		obj, err := s.remote.Stat(ctx, digest)
		obj.SizeBytes++
		return obj, err
	}))
	if err := wrongStore.Certify(t.Context(), ref, rootEvidence); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched remote object: %v", err)
	}
	if err := s.publisher.Certify(t.Context(), ref, rootEvidence); err != nil {
		t.Fatal(err)
	}
	wrongRoot := root
	wrongRoot.Offset--
	if err := s.publisher.Publish(t.Context(), ref, wrongRoot, "different locator"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed cut locator: %v", err)
	}
	if err := s.publisher.Publish(t.Context(), ref, root, "correct locator"); err != nil {
		t.Fatal(err)
	}
	if err := s.publisher.Register(t.Context(), ref, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed registered inspection: %v", err)
	}
}

func TestSavePublicationCancellationKeepsTerminalOutcome(t *testing.T) {
	for _, terminal := range []string{"cancelled", "interrupted"} {
		t.Run(terminal, func(t *testing.T) {
			f := newFixture(t)
			s := newSaveStorageFixture(t, f)
			a, save := f.finalize(t, "save")
			root, identity := s.cut(t, 5)
			f.capture(t, save, identity)
			// Cancellation/deadline settlement is an independent operation. Already
			// admitted persistence can converge while its writer is still valid.
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET status=$3,terminal_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, f.env, a.TurnID, terminal)
			if err := s.publish(t, save.ID, root); err != nil {
				t.Fatal(err)
			}
			if err := Complete(t.Context(), f.pool, f.env, f.session, a.TurnID); !errors.Is(err, ErrTerminal) {
				t.Fatalf("terminal outcome replaced: %v", err)
			}
			var state string
			if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE environment_id=$1 AND id=$2`, f.env, a.TurnID).Scan(&state); err != nil || state != terminal {
				t.Fatalf("outcome %s: %v", state, err)
			}
		})
	}
}

func TestComputerLeaseDiskRejectsChangedMountAndKey(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	key := uuid.MustParse(s.writer.ActiveKey)
	if _, err := BindComputerLeaseDisk(t.Context(), f.pool, *f.host(), f.env, f.computer, 1, key); err != nil {
		t.Fatal(err)
	}
	var seedKey uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT k.id FROM computers c JOIN computer_disk_roots r ON (r.environment_id,r.id)=(c.environment_id,c.initial_root_id) JOIN computer_data_keys k ON k.id=r.root_page_key_id WHERE c.environment_id=$1 AND c.id=$2`, f.env, f.computer).Scan(&seedKey); err != nil {
		t.Fatal(err)
	}
	if _, err := BindComputerLeaseDisk(t.Context(), f.pool, *f.host(), f.env, f.computer, 1, seedKey); !errors.Is(err, ErrDenied) {
		t.Fatalf("unowned write key: %v", err)
	}
	otherKey := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computer_data_keys(id,environment_id,writer_computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'test',decode('01','hex'))`, otherKey, f.env, f.computer)
	if _, err := BindComputerLeaseDisk(t.Context(), f.pool, *f.host(), f.env, f.computer, 1, otherKey); !errors.Is(err, ErrConflict) {
		t.Fatalf("rebound key: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET base_root_id=(SELECT id FROM computer_disk_roots WHERE environment_id=$1 AND id<>base_root_id LIMIT 1) WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
	if _, err := BindComputerLeaseDisk(t.Context(), f.pool, *f.host(), f.env, f.computer, 1, key); !errors.Is(err, ErrConflict) {
		t.Fatalf("rebound mount: %v", err)
	}
}

func TestSaveStorageSchemaRoundTrip(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	if err := schema.Down(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
}

func TestPublishedSaveReceiptRequiresOriginalHostIdentity(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	_, save := f.finalize(t, "save")
	root, identity := s.cut(t, 4)
	f.capture(t, save, identity)
	if err := s.publish(t, save.ID, root); err != nil {
		t.Fatal(err)
	}
	e := s.inspect(t, root)[0]
	other := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO worker_hosts
      SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$2::uuid,'resource_id','other-host','current_service_id',$2::uuid))).* FROM worker_hosts h WHERE id=$1`, f.worker, other)
	for _, test := range []string{"other-host", "replacement-epoch"} {
		t.Run(test, func(t *testing.T) {
			ref := s.ref(save.ID)
			if test == "other-host" {
				ref.Host.HostID = other
			} else {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET current_epoch=2 WHERE id=$1`, f.worker)
				ref.Host.Epoch = 2
			}
			if err := s.publisher.Register(t.Context(), ref, e); !errors.Is(err, ErrDenied) {
				t.Fatalf("foreign registration replay: %v", err)
			}
			if err := s.publisher.Certify(t.Context(), ref, e); !errors.Is(err, ErrDenied) {
				t.Fatalf("foreign certification replay: %v", err)
			}
			if err := s.publisher.Publish(t.Context(), ref, root, "foreign replay"); !errors.Is(err, ErrDenied) {
				t.Fatalf("foreign publication replay: %v", err)
			}
			// The internal reconciler does not borrow worker execution authority.
			if err := ReconcileSavePublication(t.Context(), f.pool, f.env, save.ID, identity); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSaveStorageClassifiesMissingAuthorityAndDependencies(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	_, save := f.finalize(t, "save")
	root, identity := s.cut(t, 6)
	f.capture(t, save, identity)
	ref := s.ref(save.ID)
	e := s.inspect(t, root)[0]
	if err := s.publisher.Certify(t.Context(), ref, e); !errors.Is(err, ErrNotReady) {
		t.Fatalf("missing registration: %v", err)
	}
	if err := s.publisher.Publish(t.Context(), ref, root, "unregistered root"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("missing root: %v", err)
	}
	unknown := ref
	unknown.SaveID = uuid.NewV7()
	if err := s.publisher.Register(t.Context(), unknown, e); !errors.Is(err, ErrDenied) {
		t.Fatalf("unknown save: %v", err)
	}
	if err := ReconcileSavePublication(t.Context(), f.pool, f.env, unknown.SaveID, identity); !errors.Is(err, ErrDenied) {
		t.Fatalf("unknown receipt: %v", err)
	}
	wrongEpoch := ref
	wrongEpoch.LeaseEpoch++
	if err := s.publisher.Register(t.Context(), wrongEpoch, e); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong epoch: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET base_root_id=NULL,write_key_id=NULL WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
	if err := s.publisher.Register(t.Context(), ref, e); !errors.Is(err, ErrDenied) {
		t.Fatalf("unbound source: %v", err)
	}
}

func TestSaveStorageRestoredSourceAndTransitiveKeys(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	_, save := f.finalize(t, "first-save")
	root, identity := s.cut(t, 3)
	f.capture(t, save, identity)
	if err := s.publish(t, save.ID, root); err != nil {
		t.Fatal(err)
	}
	key := uuid.NewV7()
	replacement := uuid.NewV7()
	nextSave := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='stopped' WHERE environment_id=$1 AND computer_id=$2;
      INSERT INTO computer_data_keys(id,environment_id,writer_computer_id,wrapping_key_id,wrapped_key) VALUES($3,$1,$2,'test',decode('02','hex'));
      INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,status,computer_instance_id,channel_credential_digest,restored_from_save_id,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
        SELECT environment_id,computer_id,2,worker_host_id,worker_epoch,clock_timestamp()+interval '1 hour','acquiring',$4,channel_credential_digest,$5::uuid,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1;
      INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq) VALUES($1,$6,$2,2,2);`, pgx.QueryExecModeSimpleProtocol, f.env, f.computer, key, replacement, save.ID, nextSave)
	bound, err := BindComputerLeaseDisk(t.Context(), f.pool, *f.host(), f.env, f.computer, 2, key)
	if err != nil || bound != root {
		t.Fatalf("restored source: %v", err)
	}
	ref := s.ref(nextSave)
	ref.LeaseEpoch = 2
	if err = s.publisher.Register(t.Context(), ref, s.inspect(t, root)[0]); !errors.Is(err, ErrDenied) {
		t.Fatalf("acquiring lease published: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='active',initialized_at=clock_timestamp() WHERE environment_id=$1 AND computer_id=$2 AND epoch=2`, f.env, f.computer)
	base, err := root.Locator(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	// A base key authorizes reading its retained graph, not encrypting new
	// objects after a new write key has been bound to this lease.
	wrongLocator, err := s.writer.Capture(t.Context(), base, 1<<20, map[uint64][]byte{0: bytes.Repeat([]byte{10}, 4096)})
	if err != nil {
		t.Fatal(err)
	}
	wrongRoot, err := disk.NewVersionRoot(wrongLocator, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.publisher.Register(t.Context(), ref, s.inspect(t, wrongRoot)[0]); !errors.Is(err, ErrConflict) {
		t.Fatalf("new object used old base key: %v", err)
	}
	s.writer.ActiveKey = key.String()
	s.writer.Keys[key.String()] = bytes.Repeat([]byte{8}, 32)
	locator, err := s.writer.Capture(t.Context(), base, 1<<20, map[uint64][]byte{0: bytes.Repeat([]byte{9}, 4096)})
	if err != nil {
		t.Fatal(err)
	}
	newRoot, err := disk.NewVersionRoot(locator, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	newIdentity, err := newRoot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err = RecordCapture(t.Context(), f.pool, CaptureEvidence{EnvironmentID: f.env, SaveID: nextSave, LeaseEpoch: 2, DiskRoot: newIdentity, Evidence: "ordered cut on admitted restored disk"}); err != nil {
		t.Fatal(err)
	}
	s.certify(t, ref, newRoot)
	if err = s.publisher.Publish(t.Context(), ref, newRoot, "both keys retained"); err != nil {
		t.Fatal(err)
	}
	var keyCount int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_object_keys WHERE environment_id=$1 AND digest=$2`, f.env, newRoot.Pack.Digest).Scan(&keyCount); err != nil || keyCount != 2 {
		t.Fatalf("transitive keys=%d %v", keyCount, err)
	}
	// Publication advanced the recovery head, but never the mounted base binding.
	if bound, err = BindComputerLeaseDisk(t.Context(), f.pool, *f.host(), f.env, f.computer, 2, key); err != nil || bound != root {
		t.Fatalf("recovery head replaced mounted source: %v", err)
	}
}

func TestSaveStorageCannotAdoptUnretainedCertifiedObjects(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	_, save := f.finalize(t, "save")
	root, identity := s.cut(t, 4)
	f.capture(t, save, identity)
	ref := s.ref(save.ID)
	inspections := s.certify(t, ref, root)
	// Dispose the old, fenced publication owner, then prepare another lease from
	// the initial source. Its predecessor's orphan graph is not that source.
	nextSave := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='all publication attempts joined' WHERE environment_id=$1 AND computer_id=$2;
      UPDATE computer_saves SET status='failed',failure_evidence='definitive no committed publication after fencing' WHERE environment_id=$1 AND id=$3;
      DELETE FROM computer_object_pins WHERE environment_id=$1 AND save_id=$3;
      INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,status,computer_instance_id,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
        SELECT environment_id,computer_id,2,worker_host_id,worker_epoch,clock_timestamp()+interval '1 hour','active',$4,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1;
      INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq) VALUES($1,$5,$2,2,2);`, pgx.QueryExecModeSimpleProtocol, f.env, f.computer, save.ID, uuid.NewV7(), nextSave)
	if _, err := BindComputerLeaseDisk(t.Context(), f.pool, *f.host(), f.env, f.computer, 2, uuid.MustParse(s.writer.ActiveKey)); err != nil {
		t.Fatal(err)
	}
	ref = s.ref(nextSave)
	ref.LeaseEpoch = 2
	if err := s.publisher.Register(t.Context(), ref, inspections[len(inspections)-1]); !errors.Is(err, ErrConflict) {
		t.Fatalf("unretained object adopted: %v", err)
	}
	var pins int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_object_pins WHERE save_id=$1`, nextSave).Scan(&pins); err != nil || pins != 0 {
		t.Fatalf("rejection leaked pins: %d %v", pins, err)
	}
}

func TestPublishedSaveReceiptSurvivesPayloadRetirement(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	a, save := f.finalize(t, "retained-receipt")
	root, identity := s.cut(t, 12)
	f.capture(t, save, identity)
	if err := s.publish(t, save.ID, root); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, a.TurnID); err != nil {
		t.Fatal(err)
	}
	var rootID uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT root_id FROM computer_saves WHERE environment_id=$1 AND id=$2`, f.env, save.ID).Scan(&rootID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE computer_saves SET root_id=NULL WHERE environment_id=$1 AND id=$2`, f.env, save.ID); err == nil {
		t.Fatal("published payload disappeared without a retirement record")
	}
	// Fixture the retention owner's eligible transition after physical stop.
	// Completed/retry evidence remains; usable payload and mounted source do not.
	dbtest.MustExec(t, t.Context(), f.pool, `
 UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='fixture physical stop' WHERE environment_id=$1 AND computer_id=$2;
 UPDATE computers SET recovery_save_id=NULL WHERE environment_id=$1 AND id=$2;
 UPDATE computer_leases SET disk_released_at=clock_timestamp() WHERE environment_id=$1 AND computer_id=$2;
 UPDATE computer_saves SET root_id=NULL,payload_retired_at=clock_timestamp() WHERE environment_id=$1 AND id=$3;
 DELETE FROM computer_object_pins WHERE environment_id=$1 AND save_id=$3;
 DELETE FROM computer_disk_roots WHERE environment_id=$1 AND id=$4;
 `, pgx.QueryExecModeSimpleProtocol, f.env, f.computer, save.ID, rootID)
	if err := s.publisher.Publish(t.Context(), s.ref(save.ID), root, "retry after retirement"); err != nil {
		t.Fatalf("published receipt depended on retired payload: %v", err)
	}
	if err := ReconcileSavePublication(t.Context(), f.pool, f.env, save.ID, identity); err != nil {
		t.Fatal(err)
	}
	other, otherIdentity := s.cut(t, 13)
	if err := s.publisher.Publish(t.Context(), s.ref(save.ID), other, "different cut"); !errors.Is(err, ErrConflict) {
		t.Fatalf("retired publication accepted different cut: %v", err)
	}
	if err := ReconcileSavePublication(t.Context(), f.pool, f.env, save.ID, otherIdentity); !errors.Is(err, ErrConflict) {
		t.Fatalf("retired receipt accepted different cut: %v", err)
	}
	if err := RecordCapture(t.Context(), f.pool, CaptureEvidence{EnvironmentID: f.env, SaveID: save.ID, LeaseEpoch: 1, DiskRoot: identity, Evidence: "capture retry"}); err != nil {
		t.Fatalf("retired receipt lost capture evidence: %v", err)
	}
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT t.status='completed' AND t.completion_save_id=s.id AND s.root_id IS NULL AND s.payload_retired_at IS NOT NULL AND octet_length(s.captured_root_digest)=32 FROM turns t JOIN computer_saves s ON (s.environment_id,s.id)=(t.environment_id,t.completion_save_id) WHERE t.environment_id=$1 AND t.id=$2`, f.env, a.TurnID).Scan(&retained); err != nil || !retained {
		t.Fatalf("terminal evidence lost: %v", err)
	}
}

func TestLeaseDiskPinsRequirePhysicalStopAndSettledPublication(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	_, save := f.finalize(t, "pending-save")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second',status='lost' WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
	if err := ReleaseComputerLeaseDisk(t.Context(), f.pool, f.env, f.computer, 1); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unfenced expired writer released its pins: %v", err)
	}
	retireKey := func() error {
		_, err := f.pool.Exec(t.Context(), `UPDATE computer_data_keys SET retired_at=clock_timestamp(),wrapped_key=NULL WHERE id=$1`, s.writer.ActiveKey)
		return err
	}
	if err := retireKey(); err == nil {
		t.Fatal("expired unfenced writer lost its write key")
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET fenced_at=clock_timestamp(),fence_evidence='fixture physical stop' WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
	if err := ReleaseComputerLeaseDisk(t.Context(), f.pool, f.env, f.computer, 1); !errors.Is(err, ErrNotReady) {
		t.Fatalf("pending save lost its source pins: %v", err)
	}
	if err := retireKey(); err == nil {
		t.Fatal("pending publication lost its write key")
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_saves SET status='failed',failure_evidence='fixture resolved absent publication' WHERE environment_id=$1 AND id=$2`, f.env, save.ID)
	if err := ReleaseComputerLeaseDisk(t.Context(), f.pool, f.env, f.computer, 1); err != nil {
		t.Fatal(err)
	}
	if err := retireKey(); err != nil {
		t.Fatalf("historical lease prevented key retirement: %v", err)
	}
	if err := ReleaseComputerLeaseDisk(t.Context(), f.pool, f.env, f.computer, 1); err != nil {
		t.Fatalf("release replay: %v", err)
	}
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT write_key_id=$3 AND base_root_id IS NOT NULL AND disk_released_at IS NOT NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, f.env, f.computer, s.writer.ActiveKey).Scan(&retained); err != nil || !retained {
		t.Fatalf("historical lease identity lost: %v", err)
	}
}

func TestLeaseDiskPinsRetainPendingCheckpointAfterSavePublication(t *testing.T) {
	f := readyCheckpointFixture(t)
	dbtest.MustExec(t, t.Context(), f.f.pool, `UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='fixture physical stop' WHERE environment_id=$1 AND computer_id=$2`, f.f.env, f.f.computer)
	if err := ReleaseComputerLeaseDisk(t.Context(), f.f.pool, f.f.env, f.f.computer, 1); !errors.Is(err, ErrNotReady) {
		t.Fatalf("pending checkpoint lost its source despite published disk: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.f.pool, `UPDATE computer_checkpoints SET status='lost',capture_request=NULL,terminal_evidence='fixture settled continuation' WHERE environment_id=$1 AND id=$2`, f.f.env, f.manifest.CheckpointID)
	if err := ReleaseComputerLeaseDisk(t.Context(), f.f.pool, f.f.env, f.f.computer, 1); err != nil {
		t.Fatalf("settled continuation retained historical lease pins: %v", err)
	}
}
