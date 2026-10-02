package controlplane

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/workerclient"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

// initialPublicationFixture is a Computer whose Instance is allocated on the
// runtest worker host and charged with its preparation, served with a local
// Computer key provider and file object storage.
type initialPublicationFixture struct {
	runtest.Fixture
	store        testUploadStore
	keys         computer.KeyWrapper
	worker       workergroup.HostPrincipal
	logicalBytes int64
	instance     pgtype.UUID
}

func newInitialPublicationFixture(t *testing.T) initialPublicationFixture {
	t.Helper()
	f := runtest.New(t)
	q := db.New(f.Pool)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.EnvironmentID, f.DeploymentID)
	c, err := q.CreateComputerFromCurrentDeployment(t.Context(), db.CreateComputerFromCurrentDeploymentParams{
		ID: pgvalue.NewUUIDv7(), InitialVersionID: pgvalue.NewUUIDv7(),
		OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID),
		DeploymentDefinitionID: pgvalue.UUID(f.ComputerDefinitionID), SandboxDeclaredID: "test-computer",
	})
	if err != nil {
		t.Fatal(err)
	}
	diskBytes := disk.SeedCapacity
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
	keys, err := computerkey.NewLocal("local-1", bytes.Repeat([]byte{0x63}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return initialPublicationFixture{Fixture: f, instance: instance.ID, store: newTestUploadStore(t), keys: keys,
		worker: workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1}, logicalBytes: diskBytes}
}

// serve serves the control plane over the fixture's database with its
// Computer key provider and object storage.
func (f initialPublicationFixture) serve(t *testing.T, configure ...func(*ServerConfig)) http.Handler {
	t.Helper()
	return newPostgresServer(t, f.Pool, append([]func(*ServerConfig){func(cfg *ServerConfig) {
		cfg.ComputerKeys = f.keys
		cfg.CAS = f.store
	}}, configure...)...)
}

// client is an authenticated worker client of the fixture's host served by
// handler.
func (f initialPublicationFixture) client(t *testing.T, handler http.Handler) *workerclient.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return seedHostSecret(t, f.Pool, f.worker.HostID).client(t, server.URL)
}

// inspectedPackDigest is the storage digest of an inspected pack.
func inspectedPackDigest(inspection blockformat.ObjectInspection) string {
	return "sha256:" + hex.EncodeToString(inspection.Pack.Pages[0].Locator.Pack.Digest[:])
}

// publishInitialVersion prepares the Instance as its worker host does:
// it certifies an empty root under the initial key and publishes it as the
// initial version with config.
func (f initialPublicationFixture) publishInitialVersion(t *testing.T, client *workerclient.Client) (workerapi.ComputerKeyMaterial, disk.VersionRoot, workerapi.InitialComputerVersionResponse) {
	t.Helper()
	key, root, _ := f.certifyInitialRoot(t, client)
	published, err := client.PublishInitialComputerVersion(t.Context(), workerapi.InitialComputerVersionRequest{ComputerInstanceID: pgvalue.UUIDString(f.instance), DesiredVersion: 1, Root: root, Config: f.initialConfig(t)})
	if err != nil {
		t.Fatalf("version publication: %v", err)
	}
	return key, root, published
}

func (f initialPublicationFixture) initialConfig(t *testing.T) oci.RuntimeConfig {
	t.Helper()
	var raw []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT s.config->'image' FROM computer_specs s JOIN computer_instances i ON i.computer_spec_id=s.id WHERE i.id=$1`, f.instance).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var config oci.RuntimeConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func (f initialPublicationFixture) prepareSeedKey(t *testing.T, client *workerclient.Client) workerapi.ComputerKeyMaterial {
	t.Helper()
	prepared, err := client.PrepareComputerSeed(t.Context(), workerapi.PrepareComputerSeedRequest{ComputerInstanceID: pgvalue.UUIDString(f.instance), DesiredVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Status != "convert" || prepared.Key == nil {
		t.Fatalf("seed preparation: %+v", prepared)
	}
	return *prepared.Key
}

// certifyInitialRoot fetches the initial key, registers an empty root,
// uploads and certifies it, and returns the root with its object request.
func (f initialPublicationFixture) certifyInitialRoot(t *testing.T, client *workerclient.Client) (workerapi.ComputerKeyMaterial, disk.VersionRoot, workerapi.InitialComputerObjectRequest) {
	t.Helper()
	instanceID := pgvalue.UUIDString(f.instance)
	key := f.prepareSeedKey(t, client)
	t.Cleanup(func() { clear(key.Key) })
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
	object := workerapi.InitialComputerObjectRequest{ComputerInstanceID: instanceID, DesiredVersion: 1, Inspection: blockformat.ObjectInspection{Pack: &inspected}}
	if err = client.RegisterInitialComputerObject(t.Context(), object); err != nil {
		t.Fatalf("object registration: %v", err)
	}
	body, err := local.Get(t.Context(), inspectedPackDigest(object.Inspection))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.Put(t.Context(), "application/octet-stream", body)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = client.CertifyInitialComputerObject(t.Context(), object); err != nil {
		t.Fatalf("object certification: %v", err)
	}
	root, err := disk.NewVersionRoot(locator, f.logicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	return key, root, object
}

// A worker host prepares an initial Instance through the authenticated
// routes: the published version becomes the Computer's head, and the
// Instance's source delivery returns it with the initial key.
func TestInitialComputerPreparationOverHTTP(t *testing.T) {
	f := newInitialPublicationFixture(t)
	client := f.client(t, f.serve(t))
	key, root, published := f.publishInitialVersion(t, client)
	var head string
	if err := f.Pool.QueryRow(t.Context(), `SELECT c.head_disk_version_id::text FROM computers c JOIN computer_instances i ON i.computer_id=c.id WHERE i.id=$1`, f.instance).Scan(&head); err != nil || head != published.VersionID {
		t.Fatalf("published head=%s response=%s err=%v", head, published.VersionID, err)
	}
	source, err := client.ComputerSource(t.Context(), workerapi.ComputerSourceRequest{ComputerInstanceID: pgvalue.UUIDString(f.instance), DesiredVersion: 1})
	if err != nil {
		t.Fatalf("source delivery: %v", err)
	}
	defer source.Clear()
	if source.VersionID != published.VersionID || source.Root != root || source.WriteKeyID == key.ID || len(source.Keys) != 2 {
		t.Fatalf("source=%s root=%v write key=%s keys=%d", source.VersionID, source.Root == root, source.WriteKeyID, len(source.Keys))
	}
	var seedKeyFound bool
	for _, material := range source.Keys {
		if material.ID == key.ID && bytes.Equal(material.Key, key.Key) {
			seedKeyFound = true
		}
	}
	if !seedKeyFound {
		t.Fatal("source missing shared seed key")
	}
	if _, err := client.ComputerSource(t.Context(), workerapi.ComputerSourceRequest{ComputerInstanceID: pgvalue.UUIDString(f.instance), DesiredVersion: 2}); err == nil {
		t.Fatal("stale source fence accepted")
	}
}

func TestComputerPreparationSourceTracksPublishedRoot(t *testing.T) {
	f := newInitialPublicationFixture(t)
	seed := initializingComputerSourceRow(t)
	// Bind a valid admitted deployment to this reserved instance. The existing
	// publication fixture's opaque candidate isolates the database protocol;
	// disk encoding/authentication is exercised by the disk package.
	dbtest.MustExec(t, t.Context(), f.Pool, `WITH lifetime AS (INSERT INTO cas_blobs (digest, size_bytes) VALUES ($2, $3) ON CONFLICT DO NOTHING) INSERT INTO cas_objects (org_id,digest,size_bytes,media_type) VALUES ($1,$2,$3,$4)`,
		f.OrgID, seed.ComputerImageDigest, seed.ComputerImageSizeBytes, seed.ComputerImageMediaType)
	seedID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO artifacts(id,org_id,project_id,environment_id,kind,digest,size_bytes,media_type)
 VALUES($1,$2,$3,$4,'computer_image',$5,$6,$7)`, seedID, f.OrgID, f.ProjectID, f.EnvironmentID, seed.ComputerImageDigest, seed.ComputerImageSizeBytes, seed.ComputerImageMediaType)
	spec, err := db.New(f.Pool).RegisterComputerSpec(t.Context(), db.RegisterComputerSpecParams{
		LogicalBytes: disk.SeedCapacity,
		ID:           pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(f.EnvironmentID),
		Config: seed.ComputerConfig, Digest: seed.ComputerSpecDigest, SeedArtifactID: pgvalue.UUID(seedID),
		SeedDigest: seed.ComputerImageDigest, SeedSizeBytes: seed.ComputerImageSizeBytes, SeedMediaType: seed.ComputerImageMediaType,
	})
	if err != nil {
		t.Fatal(err)
	}
	specID := spec.ID
	dbtest.MustExec(t, t.Context(), f.Pool, `WITH target AS (SELECT computer_id FROM computer_instances WHERE id=$1), updated AS (UPDATE computers SET computer_spec_id=$2 WHERE id=(SELECT computer_id FROM target)) UPDATE computer_instances SET computer_spec_id=$2 WHERE computer_id=(SELECT computer_id FROM target)`, f.instance, specID)
	read := func() workerapi.InstanceComputerSource {
		t.Helper()
		rows, err := db.New(f.Pool).ListComputerInstanceReconcileTargets(t.Context(), db.ListComputerInstanceReconcileTargetsParams{
			WorkerGroupID: pgvalue.UUID(f.worker.GroupID), WorkerHostID: pgvalue.UUID(f.worker.HostID),
			WorkerEpoch: f.worker.Epoch, RowLimit: 64,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("preparation rows: %d", len(rows))
		}
		source, err := projectInstanceComputerSource(rows[0])
		if err != nil {
			t.Fatal(err)
		}
		return source
	}
	initial := read()
	if initial.Seed == nil || initial.Root != nil || initial.Config.User != "1000" {
		t.Fatalf("initial: %+v", initial)
	}
	_, _, published := f.publishInitialVersion(t, f.client(t, f.serve(t)))
	continued := read()
	if continued.Seed != nil || continued.Root == nil || continued.Config.User != "1000" || continued.VersionID != initial.VersionID || continued.VersionID != published.VersionID {
		t.Fatalf("published: %+v", continued)
	}
	// A later deployment cannot replace the Computer's initial configuration.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployment_definitions SET manifest=jsonb_set(manifest,'{image,config}', '{"User":"changed"}') WHERE id=$1`, f.ComputerDefinitionID)
	if source := read(); source.Config.User != "1000" || len(source.Config.Env) != 1 || source.Config.Env[0] != "HELLO=world" || source.Root == nil {
		t.Fatalf("continuation depended on receipt or new deployment: %+v", source)
	}
}
