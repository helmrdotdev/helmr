package computer

import (
	"bytes"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// preparationFixture is a Computer whose Instance is allocated on the
// runtest worker host and charged with its preparation, with a key broker
// over a local wrapping provider and a publisher over file object storage.
type preparationFixture struct {
	runtest.Fixture
	principal    workergroup.HostPrincipal
	ref          PreparationRef
	runtime      pgtype.UUID
	logicalBytes int64
	store        *cas.File
	broker       *KeyBroker
	publisher    Publisher
}

func newPreparationFixture(t *testing.T) preparationFixture {
	t.Helper()
	f := runtest.New(t)
	q := db.New(f.Pool)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.EnvironmentID, f.DeploymentID)
	c, err := q.CreateComputerFromCurrentDeployment(t.Context(), db.CreateComputerFromCurrentDeploymentParams{
		ID: pgvalue.NewUUIDv7(), InitialVersionID: pgvalue.NewUUIDv7(),
		OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID),
		DeploymentDefinitionID: pgvalue.UUID(f.ComputerDefinitionID), SandboxDeclaredID: declaredID,
	})
	if err != nil {
		t.Fatal(err)
	}
	diskBytes := int64(compute.ComputerGuestEphemeralDiskMiB) * 1048576
	instance, err := q.AllocateComputerInstance(t.Context(), db.AllocateComputerInstanceParams{
		ID: pgvalue.NewUUIDv7(), EnvironmentID: c.EnvironmentID, ComputerID: c.ID, ComputerSpecID: c.ComputerSpecID,
		WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1,
		VMPlatformID: f.VMPlatformID, VMVCPUCount: 1, CPUConfigDigest: f.CPUConfigDigest,
		ReservedCPUMillis: 1000, ReservedMemoryBytes: 1073741824, ReservedGuestEphemeralDiskBytes: diskBytes, ReservedExecutionSlots: 1,
		PreparationSeconds: 300, WriterTtlSeconds: 600, WriterGeneration: 1, WriterTokenHash: make([]byte, 32), Reason: "computer_preparation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.ChargeComputerPreparation(t.Context(), db.ChargeComputerPreparationParams{ComputerID: c.ID, InstanceID: instance.ID}); err != nil {
		t.Fatal(err)
	}
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	local, err := computerkey.NewLocal("local-1", bytes.Repeat([]byte{0x63}, 32))
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewKeyBroker(f.Pool, local)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := NewPublisher(f.Pool, store)
	if err != nil {
		t.Fatal(err)
	}
	principal := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1}
	if err = f.Pool.QueryRow(t.Context(), `SELECT w.claim_version,g.claim_version FROM worker_hosts w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1`, f.WorkerID).Scan(&principal.HostClaimVersion, &principal.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	return preparationFixture{Fixture: f, principal: principal, ref: PreparationRef{InstanceID: pgvalue.MustUUIDValue(instance.ID), DesiredVersion: 1},
		runtime: instance.ID, logicalBytes: diskBytes, store: store, broker: broker, publisher: publisher}
}

func (f preparationFixture) runtimeWorker() any {
	return pgvalue.UUID(f.principal.HostID)
}

// initialKey delivers the fixture's initial key; the caller clears it.
func (f preparationFixture) initialKey(t *testing.T) KeyMaterial {
	t.Helper()
	key, err := f.broker.InitialKey(t.Context(), f.principal, f.ref)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// certifyInitialObject registers the object, uploads its bytes from local
// and certifies it.
func (f preparationFixture) certifyInitialObject(t *testing.T, local cas.Reader, inspection blockformat.ObjectInspection) {
	t.Helper()
	if err := f.publisher.RegisterInitialObject(t.Context(), f.principal, f.ref, inspection); err != nil {
		t.Fatal(err)
	}
	f.upload(t, local, inspection)
	if err := f.publisher.CertifyInitialObject(t.Context(), f.principal, f.ref, inspection); err != nil {
		t.Fatal(err)
	}
}

// upload copies the object's bytes from local into the publisher's storage.
func (f preparationFixture) upload(t *testing.T, local cas.Reader, inspection blockformat.ObjectInspection) {
	t.Helper()
	object, err := describeObject(inspection)
	if err != nil {
		t.Fatal(err)
	}
	body, err := local.Get(t.Context(), object.digest)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if _, err = f.store.Put(t.Context(), "application/octet-stream", body); err != nil {
		t.Fatal(err)
	}
}

// newGenerationFixture delivers the initial key, records and certifies an
// empty root under it, and returns the initial version that publishes it.
func newGenerationFixture(t *testing.T) (preparationFixture, InitialVersion) {
	t.Helper()
	f := newPreparationFixture(t)
	key := f.initialKey(t)
	defer clear(key.Key)
	local, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writer := blockformat.Writer{Source: local, Sink: local, Scope: key.Scope, ActiveKey: key.ID, Keys: map[string][]byte{key.ID: key.Key}, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(t.Context(), f.logicalBytes, 64)
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := blockformat.InspectPack(t.Context(), local, key.Scope, writer.Keys, locator.Pack)
	if err != nil {
		t.Fatal(err)
	}
	f.certifyInitialObject(t, local, blockformat.ObjectInspection{Pack: &inspected})
	root, err := disk.NewGenerationRoot(locator, f.logicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	return f, InitialVersion{Root: root, Config: oci.RuntimeConfig{User: "root", WorkingDir: "/workspace"}}
}

func requireKeyFK(t *testing.T, err error) {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23503" {
		t.Fatalf("expected FK violation: %v", err)
	}
}
