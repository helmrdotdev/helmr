package session_test

import (
	"context"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func publishRetentionImage(t *testing.T, f preparationFixture) (*preparationPublicationTest, uuid.UUID, uuid.UUID) {
	t.Helper()
	p, key := preparationPublicationTestFor(t, f)
	root := p.capture(t, key)
	if err := p.publisher.Publish(t.Context(), *f.host(), p.ref, root, "retained publication evidence"); err != nil {
		t.Fatal(err)
	}
	var image, waiter uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT i.id,c.id FROM images i JOIN computers c ON (c.environment_id,c.image_build_id)=(i.environment_id,i.image_build_id) WHERE i.environment_id=$1 AND i.image_build_id=$2`, f.env, p.ref.ImageBuildID).Scan(&image, &waiter); err != nil {
		t.Fatal(err)
	}
	return p, image, waiter
}

func stopRetentionBuild(t *testing.T, p *preparationPublicationTest) {
	t.Helper()
	if err := computer.ObserveImageBuildStopped(t.Context(), p.f.pool, *p.f.host(), preparationIdentity(p.ref)); err != nil {
		t.Fatal(err)
	}
}

func reconcileImageRetention(t *testing.T, f preparationFixture) {
	t.Helper()
	if err := computer.ReconcileDiskRetention(t.Context(), f.pool); err != nil {
		t.Fatal(err)
	}
}

func assertImagePayload(t *testing.T, f preparationFixture, image uuid.UUID, retained bool) {
	t.Helper()
	var payload, history bool
	if err := f.pool.QueryRow(t.Context(), `SELECT i.root_id IS NOT NULL,i.publication_evidence IS NOT NULL AND p.capture_root IS NOT NULL
 AND EXISTS(SELECT 1 FROM secret_exposures x WHERE x.environment_id=p.environment_id AND x.image_build_id=p.id)
 FROM images i JOIN image_builds p ON (p.environment_id,p.id)=(i.environment_id,i.image_build_id) WHERE i.environment_id=$1 AND i.id=$2`, f.env, image).Scan(&payload, &history); err != nil || payload != retained || !history {
		t.Fatalf("image payload=%v want=%v history=%v error=%v", payload, retained, history, err)
	}
}

func TestSupersededImageRetainsPinsAndNewestThenReclaimsDeletedComputerPayload(t *testing.T) {
	f := newPreparationFixture(t)
	first, oldImage, oldComputer := publishRetentionImage(t, f)
	if got, err := computer.PinImage(t.Context(), f.pool, f.env, oldComputer); err != nil || got.ID != oldImage {
		t.Fatalf("initial pin: %+v %v", got, err)
	}
	stopRetentionBuild(t, first)
	second, newest, _ := publishRetentionImage(t, f)
	stopRetentionBuild(t, second)
	reconcileImageRetention(t, f)
	assertImagePayload(t, f, oldImage, true)
	assertImagePayload(t, f, newest, true)
	if got, err := computer.PinImage(t.Context(), f.pool, f.env, oldComputer); err != nil || got.ID != oldImage {
		t.Fatalf("refresh changed existing pin: %+v %v", got, err)
	}
	fresh := f.waiter(t)
	if got, err := computer.PinImage(t.Context(), f.pool, f.env, fresh); err != nil || got.ID != newest {
		t.Fatalf("fresh Computer missed latest image: %+v %v", got, err)
	}
	scope := computer.Scope{EnvironmentID: f.env}
	if err := f.pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.env).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := computer.Delete(t.Context(), f.pool, computer.Deletion{Principal: sessiontestPrincipal(t, f.fixture), Scope: scope, ComputerID: oldComputer, IdempotencyKey: "delete-old-pin"}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		reconcileImageRetention(t, f)
	}
	assertImagePayload(t, f, oldImage, false)
	assertImagePayload(t, f, newest, true)
}

func TestSupersededImageKeepsEligibleAttachedWaiter(t *testing.T) {
	f := newPreparationFixture(t)
	first, oldImage, waiter := publishRetentionImage(t, f)
	stopRetentionBuild(t, first)
	second, newest, _ := publishRetentionImage(t, f)
	stopRetentionBuild(t, second)
	reconcileImageRetention(t, f)
	assertImagePayload(t, f, oldImage, true)
	// Attached demand must resolve its original chain even when a fresher
	// unrelated refresh image now exists.
	got, err := computer.PinImage(t.Context(), f.pool, f.env, waiter)
	if err != nil || got.ID != oldImage {
		t.Fatalf("retention broke attached image selection: %+v %v", got, err)
	}
	assertImagePayload(t, f, newest, true)
}

func TestSupersededImageRetiresExpiredWaiterButRequiresPhysicalStop(t *testing.T) {
	f := newPreparationFixture(t)
	p, image, waiter := publishRetentionImage(t, f)
	if _, err := f.secrets.Rotate(t.Context(), audit.System("test-fixture"), f.env, f.secretID, []byte("v2"), "obsolete-image"); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET preparation_deadline_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.env, waiter)
	reconcileImageRetention(t, f)
	assertImagePayload(t, f, image, true)
	stopRetentionBuild(t, p)
	for range 3 {
		reconcileImageRetention(t, f)
	}
	assertImagePayload(t, f, image, false)
}

func TestSupersededImageRetainsPinCommittedBeforeConcurrentPublication(t *testing.T) {
	f := newPreparationFixture(t)
	first, oldImage, oldWaiter := publishRetentionImage(t, f)
	stopRetentionBuild(t, first)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET preparation_failed_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, f.env, oldWaiter)
	second, key := preparationPublicationTestFor(t, f)
	root := second.capture(t, key)
	fresh := f.waiter(t)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); _ = tx.Rollback(context.Background()); wg.Wait() })
	got, err := computer.PinAdmissionImage(ctx, tx, f.env, fresh)
	if err != nil || got.ID != oldImage {
		t.Fatalf("pin before publication: %+v %v", got, err)
	}
	// Retirement must skip the spec while the new pin is still uncommitted.
	reconcileImageRetention(t, f)
	assertImagePayload(t, f, oldImage, true)
	done := make(chan error, 1)
	wg.Go(func() { done <- second.publisher.Publish(ctx, *f.host(), second.ref, root, "concurrent publication") })
	waitSessionLifecycleLock(t, f.fixture)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stopRetentionBuild(t, second)
	reconcileImageRetention(t, f)
	assertImagePayload(t, f, oldImage, true)
	if pinned, err := computer.PinImage(ctx, f.pool, f.env, fresh); err != nil || pinned.ID != oldImage {
		t.Fatalf("retention lost the committed pin: %+v %v", pinned, err)
	}
}

func TestSupersededImageKeepsAttachedSuccessorBehindNewerRefresh(t *testing.T) {
	f := newPreparationFixture(t)
	p, oldImage, waiter := publishRetentionImage(t, f)
	stopRetentionBuild(t, p)
	if _, err := f.secrets.Rotate(t.Context(), audit.System("test-fixture"), f.env, f.secretID, []byte("v2"), "successor-inputs"); err != nil {
		t.Fatal(err)
	}
	next, err := computer.EnsureImageBuildSuccessor(t.Context(), f.pool, f.env, p.ref.ImageBuildID)
	if err != nil {
		t.Fatal(err)
	}
	p.ref = f.claim(t, next)
	broker, err := computer.NewImageBuildKeyBroker(f.pool, preparationWrapper(t))
	if err != nil {
		t.Fatal(err)
	}
	key, err := broker.WriteKey(t.Context(), *f.host(), p.ref)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Key)
	root := p.capture(t, key)
	if err := p.publisher.Publish(t.Context(), *f.host(), p.ref, root, "attached successor"); err != nil {
		t.Fatal(err)
	}
	var successorImage uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM images WHERE environment_id=$1 AND image_build_id=$2`, f.env, next.ID).Scan(&successorImage); err != nil {
		t.Fatal(err)
	}
	stopRetentionBuild(t, p)
	newer, newestImage, _ := publishRetentionImage(t, f)
	stopRetentionBuild(t, newer)
	reconcileImageRetention(t, f)
	assertImagePayload(t, f, successorImage, true)
	got, err := computer.PinImage(t.Context(), f.pool, f.env, waiter)
	if err != nil || got.ID != successorImage {
		t.Fatalf("retirement broke attached successor behind refresh: %+v %v", got, err)
	}
	reconcileImageRetention(t, f)
	assertImagePayload(t, f, oldImage, false)
	assertImagePayload(t, f, successorImage, true)
	assertImagePayload(t, f, newestImage, true)
}

func TestSupersededImageDiscoverySkipsBusySecretBeforeBatchBound(t *testing.T) {
	f := newPreparationFixture(t)
	var blocked uuid.UUID
	for n := 0; n < 101; n++ {
		p, image, waiter := publishRetentionImage(t, f)
		stopRetentionBuild(t, p)
		if n == 0 {
			blocked = image
		}
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET preparation_failed_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, f.env, waiter)
	}
	independent := f.fixture
	independent.deployment = uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO deployments(environment_id,id,bundle_digest) SELECT environment_id,$3,'sha256:'||repeat('9',64) FROM deployments WHERE environment_id=$1 AND id=$2;
 INSERT INTO image_specs(environment_id,id,spec_digest,spec,seed) SELECT environment_id,$3,'sha256:'||repeat('9',64),spec,seed FROM image_specs WHERE environment_id=$1 AND id=$2;
 INSERT INTO templates(environment_id,id,key) VALUES($1,$3,'independent-retention');
 INSERT INTO template_definitions(environment_id,deployment_id,template_id,image_spec_id,cpu_millis,memory_bytes,disk_bytes) SELECT environment_id,$3,$3,$3,cpu_millis,memory_bytes,disk_bytes FROM template_definitions WHERE environment_id=$1 AND deployment_id=$2`, pgx.QueryExecModeSimpleProtocol, f.env, f.deployment, independent.deployment)
	later := preparationFixtureFor(t, independent)
	p, old, waiter := publishRetentionImage(t, later)
	stopRetentionBuild(t, p)
	dbtest.MustExec(t, t.Context(), later.pool, `UPDATE computers SET preparation_failed_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, later.env, waiter)
	newest, _, _ := publishRetentionImage(t, later)
	stopRetentionBuild(t, newest)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SELECT id FROM secrets WHERE environment_id=$1 AND id=$2 FOR UPDATE`, f.env, f.secretID)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for range 2 {
		if err := computer.ReconcileDiskRetention(ctx, f.pool); err != nil {
			t.Fatal(err)
		}
	}
	assertImagePayload(t, f, blocked, true)
	assertImagePayload(t, later, old, false)
}
