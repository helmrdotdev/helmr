package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

type preparationPublicationTest struct {
	f         preparationFixture
	ref       PreparationExecutor
	publisher *PreparationPublisher
	remote    *cas.File
	inspected []blockformat.ObjectInspection
}

func (p *preparationPublicationTest) Register(ctx context.Context, e blockformat.ObjectInspection) error {
	p.inspected = append(p.inspected, e)
	return p.publisher.Register(ctx, *p.f.host(), p.ref, e)
}
func (p *preparationPublicationTest) Certify(ctx context.Context, e blockformat.ObjectInspection) error {
	return p.publisher.Certify(ctx, *p.f.host(), p.ref, e)
}
func (p *preparationPublicationTest) Upload(ctx context.Context, d cas.Descriptor, f *os.File) (cas.Object, error) {
	if err := cas.VerifyDescriptorFile(ctx, d, f); err != nil {
		return cas.Object{}, err
	}
	return p.remote.Put(ctx, d.MediaType, io.NewSectionReader(f, 0, d.SizeBytes))
}
func newPreparationPublicationTest(t *testing.T) (*preparationPublicationTest, PreparationKey) {
	t.Helper()
	return preparationPublicationTestFor(t, newPreparationFixture(t))
}
func preparationPublicationTestFor(t *testing.T, f preparationFixture) (*preparationPublicationTest, PreparationKey) {
	t.Helper()
	ref := f.claim(t, f.attach(t, f.waiter(t)))
	remote, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := NewPreparationPublisher(f.pool, remote)
	if err != nil {
		t.Fatal(err)
	}
	broker, _ := NewPreparationKeyBroker(f.pool, preparationWrapper(t))
	key, err := broker.WriteKey(t.Context(), *f.host(), ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(key.Key) })
	return &preparationPublicationTest{f: f, ref: ref, remote: remote, publisher: publisher}, key
}
func (p *preparationPublicationTest) capture(t *testing.T, key PreparationKey) disk.VersionRoot {
	t.Helper()
	return p.captureCapacity(t, key, disk.SeedCapacity)
}
func (p *preparationPublicationTest) captureCapacity(t *testing.T, key PreparationKey, capacity int64) disk.VersionRoot {
	t.Helper()
	if _, err := RecordPreparationExposure(t.Context(), p.f.pool, *p.f.host(), p.ref); err != nil {
		t.Fatal(err)
	}
	if err := p.publisher.BeginCapture(t.Context(), *p.f.host(), p.ref, capacity); err != nil {
		t.Fatal(err)
	}
	if values, err := RecordPreparationExposure(t.Context(), p.f.pool, *p.f.host(), p.ref); !errors.Is(err, ErrDenied) || values != nil {
		t.Fatalf("sealed delivery: %v", err)
	}
	file, err := os.CreateTemp(t.TempDir(), "disk")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err = file.Truncate(capacity); err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt(bytes.Repeat([]byte{29}, 4096), 4096); err != nil {
		t.Fatal(err)
	}
	candidate, err := disk.CaptureInitialVersion(t.Context(), disk.VersionCapture{Disk: file, Capacity: capacity, StagingParent: t.TempDir(), Scope: key.Scope, KeyID: key.ID.String(), Key: key.Key, Fanout: 64, PackLimit: blockformat.MinPackLimit, MaxStagedBytes: 4 << 20, MaxObjects: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	locator, err := candidate.Publish(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := blockformat.OpenTree(t.Context(), p.remote, key.Scope, map[string][]byte{key.ID.String(): key.Key}, locator)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := tree.ReadBlock(t.Context(), 1)
	if err != nil || !bytes.Equal(actual, bytes.Repeat([]byte{29}, 4096)) {
		t.Fatalf("disk content: %v", err)
	}
	root, err := disk.NewVersionRoot(locator, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.publisher.Capture(t.Context(), *p.f.host(), p.ref, root, "host completed quiescent full disk cut"); err != nil {
		t.Fatal(err)
	}
	return root
}
func TestPreparationPublicationFullGraphAndReplay(t *testing.T) {
	p, key := newPreparationPublicationTest(t)
	if err := p.publisher.BeginCapture(t.Context(), *p.f.host(), p.ref, disk.SeedCapacity); !errors.Is(err, ErrNotReady) {
		t.Fatalf("capture before exposure: %v", err)
	}
	root := p.capture(t, key)
	if err := p.publisher.BeginCapture(t.Context(), *p.f.host(), p.ref, 2<<20); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("capacity changed: %v", err)
	}
	// Ordinary rotation does not turn a successful cut into an execution failure.
	if _, err := p.f.secrets.Rotate(t.Context(), p.f.env, p.f.secretID, []byte("v2"), "during-capture"); err != nil {
		t.Fatal(err)
	}
	swapped := root
	swapped.Page.Digest = "sha256:" + strings.Repeat("a", 64)
	if _, err := swapped.Digest(); err != nil {
		t.Fatal(err)
	}
	if err := p.publisher.Capture(t.Context(), *p.f.host(), p.ref, swapped, "different page in same pack"); !errors.Is(err, ErrConflict) {
		t.Fatalf("capture changed page: %v", err)
	}
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, swapped, "different page in same pack"); !errors.Is(err, ErrConflict) {
		t.Fatalf("publication changed page: %v", err)
	}
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "verified storage graph"); err != nil {
		t.Fatal(err)
	}
	var succeeded, unfenced bool
	if err := p.f.pool.QueryRow(t.Context(), `SELECT status='succeeded',fenced_at IS NULL FROM computer_preparations WHERE environment_id=$1 AND id=$2`, p.f.env, p.ref.PreparationID).Scan(&succeeded, &unfenced); err != nil || !succeeded || !unfenced {
		t.Fatalf("publication fenced executor: %v", err)
	}
	if err := ObservePreparationStopped(t.Context(), p.f.pool, *p.f.host(), p.ref.Identity()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), p.f.pool, `UPDATE computer_preparations SET executor_expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, p.f.env, p.ref.PreparationID)
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "lost response replay"); err != nil {
		t.Fatal(err)
	}
	for _, e := range p.inspected {
		if err := p.publisher.Register(t.Context(), *p.f.host(), p.ref, e); err != nil {
			t.Fatal(err)
		}
		if err := p.publisher.Certify(t.Context(), *p.f.host(), p.ref, e); err != nil {
			t.Fatal(err)
		}
	}
	changed := root
	changed.LogicalBytes *= 2
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, changed, "different cut"); err == nil {
		t.Fatal("changed root accepted")
	}
	if _, err := p.f.pool.Exec(t.Context(), `DELETE FROM computer_objects WHERE environment_id=$1 AND digest=$2`, p.f.env, root.Pack.Digest); err == nil {
		t.Fatal("retained root deleted")
	}
}
func TestPreparationPublicationImageSequence(t *testing.T) {
	p, key := newPreparationPublicationTest(t)
	for want := int64(1); want <= 2; want++ {
		root := p.capture(t, key)
		if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified image"); err != nil {
			t.Fatal(err)
		}
		if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "replay"); err != nil {
			t.Fatal(err)
		}
		var seq, count int64
		if err := p.f.pool.QueryRow(t.Context(), `SELECT seq,(SELECT count(*) FROM computer_images WHERE environment_id=$1) FROM computer_images WHERE environment_id=$1 AND preparation_id=$2`, p.f.env, p.ref.PreparationID).Scan(&seq, &count); err != nil || seq != want || count != want {
			t.Fatalf("image sequence %d count %d: %v", seq, count, err)
		}
		if want == 2 {
			break
		}
		if err := ObservePreparationStopped(t.Context(), p.f.pool, *p.f.host(), p.ref.Identity()); err != nil {
			t.Fatal(err)
		}
		p.ref = p.f.claim(t, p.f.attach(t, p.f.waiter(t)))
		broker, _ := NewPreparationKeyBroker(p.f.pool, preparationWrapper(t))
		var err error
		key, err = broker.WriteKey(t.Context(), *p.f.host(), p.ref)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(key.Key)
	}
}

func TestPreparationPublicationRequiresLiveExecutor(t *testing.T) {
	for _, change := range []string{"stop", "expire", "revoke"} {
		t.Run(change, func(t *testing.T) {
			p, key := newPreparationPublicationTest(t)
			root := p.capture(t, key)
			switch change {
			case "stop":
				if err := ObservePreparationStopped(t.Context(), p.f.pool, *p.f.host(), p.ref.Identity()); err != nil {
					t.Fatal(err)
				}
			case "expire":
				dbtest.MustExec(t, t.Context(), p.f.pool, `UPDATE computer_preparations SET executor_expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, p.f.env, p.ref.PreparationID)
			case "revoke":
				if _, err := p.f.secrets.Revoke(t.Context(), p.f.env, p.f.secretID, "revoke"); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "late publish"); !errors.Is(err, ErrDenied) {
				t.Fatalf("late publication: %v", err)
			}
			var count int
			if err := p.f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_images WHERE environment_id=$1 AND preparation_id=$2`, p.f.env, p.ref.PreparationID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("image leaked %d: %v", count, err)
			}
		})
	}
}

func TestPreparationCertificationRechecksStorageIO(t *testing.T) {
	p, key := newPreparationPublicationTest(t)
	if _, err := RecordPreparationExposure(t.Context(), p.f.pool, *p.f.host(), p.ref); err != nil {
		t.Fatal(err)
	}
	if err := p.publisher.BeginCapture(t.Context(), *p.f.host(), p.ref, disk.SeedCapacity); err != nil {
		t.Fatal(err)
	}
	writer := blockformat.Writer{Source: p.remote, Sink: p.remote, Scope: key.Scope, ActiveKey: key.ID.String(), Keys: map[string][]byte{key.ID.String(): key.Key}, PackLimit: blockformat.MinPackLimit}
	root, err := writer.Empty(t.Context(), disk.SeedCapacity, 64)
	if err != nil {
		t.Fatal(err)
	}
	pack, err := blockformat.InspectPack(t.Context(), p.remote, key.Scope, writer.Keys, root.Pack)
	if err != nil {
		t.Fatal(err)
	}
	inspection := blockformat.ObjectInspection{Pack: &pack}
	if err = p.Register(t.Context(), inspection); err != nil {
		t.Fatal(err)
	}
	guarded, _ := NewPreparationPublisher(p.f.pool, saveStatFunc(func(ctx context.Context, digest string) (cas.Object, error) {
		// A separate transaction must complete while Stat is in flight.
		revokeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := p.f.secrets.Revoke(revokeCtx, p.f.env, p.f.secretID, "during-stat"); err != nil {
			return cas.Object{}, err
		}
		return p.remote.Stat(ctx, digest)
	}))
	if err = guarded.Certify(t.Context(), *p.f.host(), p.ref, inspection); !errors.Is(err, ErrDenied) {
		t.Fatalf("certified after revocation: %v", err)
	}
	object, _ := describeSaveObject(inspection)
	var certified bool
	if err = p.f.pool.QueryRow(t.Context(), `SELECT certified FROM computer_objects WHERE environment_id=$1 AND digest=$2`, p.f.env, object.digest).Scan(&certified); err != nil || certified {
		t.Fatalf("certified stale object: %v", err)
	}
}

func TestPreparationPublicationRejectsUnrelatedCertifiedObject(t *testing.T) {
	p, _ := newPreparationPublicationTest(t)
	unrelated := newSaveStorageFixture(t, p.f.fixture)
	if _, err := RecordPreparationExposure(t.Context(), p.f.pool, *p.f.host(), p.ref); err != nil {
		t.Fatal(err)
	}
	if err := p.publisher.BeginCapture(t.Context(), *p.f.host(), p.ref, disk.SeedCapacity); err != nil {
		t.Fatal(err)
	}
	pack, err := blockformat.InspectPack(t.Context(), unrelated.local, unrelated.writer.Scope, unrelated.writer.Keys, unrelated.base.Pack)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.publisher.Register(t.Context(), *p.f.host(), p.ref, blockformat.ObjectInspection{Pack: &pack}); !errors.Is(err, ErrConflict) {
		t.Fatalf("adopted foreign certified graph: %v", err)
	}
	var count int
	if err = p.f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_object_pins WHERE environment_id=$1 AND preparation_id=$2`, p.f.env, p.ref.PreparationID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retained unauthorized graph: %d %v", count, err)
	}
}
