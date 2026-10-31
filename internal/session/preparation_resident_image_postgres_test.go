package session_test

import (
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/dispatch"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"

	"github.com/jackc/pgx/v5"
)

func preparationResidentImage(t *testing.T) *preparationPublicationTest {
	t.Helper()
	p, key := newPreparationPublicationTest(t)
	root := p.capture(t, key)
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified"); err != nil {
		t.Fatal(err)
	}
	// Install the real published image through the admission operation; the
	// resident fixture represents the later allocator-created process and lease.
	dbtest.MustExec(t, t.Context(), p.f.pool, `UPDATE computers SET initial_root_id=NULL,initial_root_digest=NULL,image_spec_id=$3,preparation_deadline_at=clock_timestamp()+interval '5 minutes' WHERE environment_id=$1 AND id=$2`, p.f.env, p.f.computer, p.f.deployment)
	if _, err := computer.PinImage(t.Context(), p.f.pool, p.f.env, p.f.computer); err != nil {
		t.Fatal(err)
	}
	return p
}

// Revoking a Secret prepared into a running Computer's image stops only its
// provision: the lease keeps renewing and the running process is not stopped.
func TestPreparationImageRevocationKeepsRunningComputer(t *testing.T) {
	p := preparationResidentImage(t)
	f := p.f.fixture
	var instanceID string
	if err := f.pool.QueryRow(t.Context(), `SELECT allocation_id::text FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, f.env, f.computer).Scan(&instanceID); err != nil {
		t.Fatal(err)
	}
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, Epoch: 1, InstanceID: uuid.MustParse(instanceID)}
	if _, err := p.f.secrets.Revoke(t.Context(), dbtest.SecretManager(t, f.pool, f.env), f.env, p.f.secretID, "revoke"); err != nil {
		t.Fatal(err)
	}
	if _, err := computer.RenewLease(t.Context(), f.pool, *f.host(), identity); err != nil {
		t.Fatalf("revocation stopped lease renewal: %v", err)
	}
	if _, _, err := dispatch.ReconcileImageBuildLifecycle(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, dispatch.ImageBuildLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	var running bool
	if err := f.pool.QueryRow(t.Context(), `SELECT p.status IN ('starting','ready') AND p.failure_recorded_at IS NULL AND NOT EXISTS(SELECT 1 FROM session_holds h WHERE h.environment_id=p.environment_id AND h.session_id=p.session_id) FROM session_processes p WHERE p.environment_id=$1 AND p.session_id=$2`, f.env, f.session).Scan(&running); err != nil || !running {
		t.Fatalf("revocation stopped the running process: %v %v", running, err)
	}
}

// Bootstrap lineage on the authenticated initial root from the existing
// checkpoint fixture. Publication/key provenance is proved by the separate real
// capture tests; this fixture exercises the later restore/control transitions.
func checkpointImageExposure(t *testing.T, f fixture) preparationFixture {
	t.Helper()
	p := preparationFixtureFor(t, f)
	attempt := p.attach(t, p.waiter(t))
	ref := p.claim(t, attempt)
	if _, err := computer.RecordImageBuildExposure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), ref); err != nil {
		t.Fatal(err)
	}
	broker, _ := computer.NewImageBuildKeyBroker(f.pool, preparationWrapper(t))
	key, err := broker.WriteKey(t.Context(), *f.host(), ref)
	if err != nil {
		t.Fatal(err)
	}
	clear(key.Key)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds p SET status='succeeded',logical_bytes=1048576,capture_root='sha256:'||encode(c.initial_root_digest,'hex'),capture_evidence='fixture authenticated root' FROM computers c WHERE p.environment_id=$1 AND p.id=$2 AND c.environment_id=$1 AND c.id=$3;
 INSERT INTO images(environment_id,id,image_spec_id,image_build_id,seq,root_id,publication_evidence) SELECT $1,$2,$4,$2,1,initial_root_id,'fixture lineage' FROM computers WHERE environment_id=$1 AND id=$3;
 UPDATE computers SET image_spec_id=$4,image_id=$2 WHERE environment_id=$1 AND id=$3;`, pgx.QueryExecModeSimpleProtocol, f.env, attempt.ID, f.computer, f.deployment)
	return p
}
