package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

// The executor reservation is fixture state. Requests use real authenticated
// worker transport and the same domain transactions as production allocations.
func preparationTransportFixture(t *testing.T) (agenttest.Fixture, workerapi.PreparationExecutor) {
	t.Helper()
	f := agenttest.New(t)
	request := workerapi.PreparationExecutor{Identity: workerapi.AllocationIdentity{Kind: "preparation", EnvironmentID: f.Environment.String(), OwnerID: uuid.NewV7().String(), InstanceID: uuid.NewV7().String(), Epoch: 1}, ChannelCredential: bytes.Repeat([]byte{21}, 32)}
	digest := sha256.Sum256(request.ChannelCredential)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_preparations(environment_id,id,preparation_spec_id,retry_key,status,deadline_at,executor_epoch,worker_host_id,worker_epoch,instance_id,channel_credential_digest,executor_expires_at,delivered_at,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest)
 SELECT $1,$2,$3,'transport','running',clock_timestamp()+interval '10 minutes',1,id,1,$4,$5,clock_timestamp()+interval '1 minute',clock_timestamp(),1000,536870912,1073741824,vm_platform_id,1,cpu_environment_digest FROM worker_hosts WHERE id=$6`, f.Environment, request.Identity.OwnerID, f.Deployment, request.Identity.InstanceID, digest[:], f.Worker)
	return f, request
}

func TestWorkerPreparationAuthorityAndTerminalCustody(t *testing.T) {
	f, request := preparationTransportFixture(t)
	wrapper, err := computerkey.NewLocal("preparation-transport-test", bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newPostgresServer(t, f.Pool, func(cfg *ServerConfig) { cfg.ComputerKeys = wrapper }))
	defer server.Close()
	auth := seedHostSecret(t, f.Pool, f.Worker)
	client := auth.client(t, server.URL)
	renewed, err := client.RenewPreparation(t.Context(), request)
	if err != nil || renewed.ExpiresAt.IsZero() {
		t.Fatalf("renewal: %+v %v", renewed, err)
	}
	key, err := client.PreparationWriteKey(t.Context(), request)
	if err != nil || len(key.Key) != 32 {
		t.Fatalf("key: %v", err)
	}
	defer clear(key.Key)
	replay, err := client.PreparationWriteKey(t.Context(), request)
	if err != nil || key.ID != replay.ID || !bytes.Equal(key.Key, replay.Key) {
		t.Fatalf("key replay: %v", err)
	}
	clear(replay.Key)
	body, _ := json.Marshal(request)
	response := postAgentComputerJSON(t, server, auth, "/worker/v1/allocations/preparation/key", body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("key response was cacheable or failed")
	}
	for _, mutate := range []func(*workerapi.PreparationExecutor){
		func(r *workerapi.PreparationExecutor) { r.Identity.Kind = "computer" },
		func(r *workerapi.PreparationExecutor) { r.Identity.Epoch++ },
		func(r *workerapi.PreparationExecutor) { r.Identity.InstanceID = uuid.NewV7().String() },
		func(r *workerapi.PreparationExecutor) { r.ChannelCredential = bytes.Repeat([]byte{22}, 32) },
	} {
		wrong := request
		mutate(&wrong)
		if leaked, err := client.PreparationWriteKey(t.Context(), wrong); err == nil || len(leaked.Key) > 0 {
			clear(leaked.Key)
			t.Fatal("foreign executor received a key")
		}
		if _, err := client.RenewPreparation(t.Context(), wrong); err == nil {
			t.Fatal("foreign executor renewed")
		}
		if err := client.BeginPreparationCapture(t.Context(), workerapi.PreparationCaptureBegin{Executor: wrong, LogicalBytes: disk.SeedCapacity}); err == nil {
			t.Fatal("foreign executor sealed capture")
		}
	}
	if err := client.FailPreparation(t.Context(), workerapi.PreparationFailure{Executor: request, Code: "authored_preparation_failed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RenewPreparation(t.Context(), request); err == nil {
		t.Fatal("failed executor renewed")
	}
	if leaked, err := client.PreparationWriteKey(t.Context(), request); err == nil || len(leaked.Key) > 0 {
		clear(leaked.Key)
		t.Fatal("failed executor received key")
	}
	var unfenced bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT fenced_at IS NULL FROM computer_preparations WHERE environment_id=$1 AND id=$2`, f.Environment, request.Identity.OwnerID).Scan(&unfenced); err != nil || !unfenced {
		t.Fatalf("failure released custody: %v", err)
	}
	if err := client.ObservePreparationStopped(t.Context(), request.Identity); err != nil {
		t.Fatal(err)
	}
	if err := client.FailPreparation(t.Context(), workerapi.PreparationFailure{Executor: request, Code: "late_report"}); err != nil {
		t.Fatal(err)
	}
}

type preparationSecretOpener struct {
	opener SecretDeliveryOpener
	after  func()
}

func (o *preparationSecretOpener) OpenDeliveries(env uuid.UUID, envelopes []secret.DeliveryEnvelope) ([]secret.DeliveryMaterial, error) {
	values, err := o.opener.OpenDeliveries(env, envelopes)
	if o.after != nil {
		o.after()
	}
	return values, err
}

func TestWorkerPreparationSecretsPinBeforeOpeningAndRecheck(t *testing.T) {
	for _, transition := range []string{"expiry", "seal", "revocation"} {
		for _, kind := range []string{"env", "file"} {
			t.Run(transition+"/"+kind, func(t *testing.T) {
				f, request := preparationTransportFixture(t)
				secrets, err := secret.New(db.New(f.Pool), f.Pool, bytes.Repeat([]byte{17}, 32))
				if err != nil {
					t.Fatal(err)
				}
				record, err := secrets.Create(t.Context(), f.Environment, "BUILD_TOKEN", []byte("first"), "create")
				if err != nil {
					t.Fatal(err)
				}
				secretID := pgvalue.MustUUIDValue(record.ID)
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_secret_bindings(environment_id,preparation_spec_id,secret_id,placement_kind,placement_target,mode) VALUES($1,$2,$3,$4,$5,'raw')`, f.Environment, f.Deployment, secretID, kind, map[string]string{"env": "TOKEN", "file": "/etc/service/token"}[kind])
				opener := &preparationSecretOpener{opener: secrets}
				server := httptest.NewServer(newPostgresServer(t, f.Pool, func(cfg *ServerConfig) { cfg.SecretDelivery = opener }))
				defer server.Close()
				client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
				opener.after = func() {
					var count int
					if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM secret_exposures WHERE environment_id=$1 AND preparation_id=$2`, f.Environment, request.Identity.OwnerID).Scan(&count); err != nil || count != 1 {
						t.Errorf("exposure not committed before opening: %d %v", count, err)
					}
				}
				first, err := client.PreparationSecrets(t.Context(), request)
				if err != nil || len(first.Secrets) != 1 || string(first.Secrets[0].Value) != "first" {
					t.Fatalf("first delivery: %v", err)
				}
				if kind == "env" && (first.Secrets[0].Env == nil || first.Secrets[0].Env.Name != "TOKEN" || first.Secrets[0].File != nil) {
					t.Fatal("environment placement lost")
				}
				if kind == "file" && (first.Secrets[0].File == nil || first.Secrets[0].File.Path != "/etc/service/token" || first.Secrets[0].Env != nil) {
					t.Fatal("file placement lost")
				}
				clear(first.Secrets[0].Value)
				if _, err := secrets.Rotate(t.Context(), f.Environment, secretID, []byte("second"), "rotate"); err != nil {
					t.Fatal(err)
				}
				replay, err := client.PreparationSecrets(t.Context(), request)
				if err != nil || len(replay.Secrets) != 1 || string(replay.Secrets[0].Value) != "first" {
					t.Fatalf("exposed version changed: %v", err)
				}
				clear(replay.Secrets[0].Value)
				opener.after = func() {
					switch transition {
					case "expiry":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_preparations SET executor_expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.Environment, request.Identity.OwnerID)
					case "seal":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_preparations SET logical_bytes=$3 WHERE environment_id=$1 AND id=$2`, f.Environment, request.Identity.OwnerID, disk.SeedCapacity)
					case "revocation":
						if _, err := secrets.Revoke(t.Context(), f.Environment, secretID, "revoke"); err != nil {
							t.Error(err)
						}
					}
				}
				if result, err := client.PreparationSecrets(t.Context(), request); err == nil || len(result.Secrets) != 0 {
					t.Fatal("authority loss during decryption returned secret material")
				}
			})
		}
	}
}

type preparationHTTPPublication struct {
	client   *workerclient.Client
	executor workerapi.PreparationExecutor
	store    testUploadStore
}

func (p preparationHTTPPublication) Register(ctx context.Context, inspection blockformat.ObjectInspection) error {
	return p.client.RegisterPreparationObject(ctx, workerapi.PreparationObject{Executor: p.executor, Inspection: inspection})
}
func (p preparationHTTPPublication) Certify(ctx context.Context, inspection blockformat.ObjectInspection) error {
	return p.client.CertifyPreparationObject(ctx, workerapi.PreparationObject{Executor: p.executor, Inspection: inspection})
}
func (p preparationHTTPPublication) Upload(ctx context.Context, d cas.Descriptor, file *os.File) (cas.Object, error) {
	if err := cas.VerifyDescriptorFile(ctx, d, file); err != nil {
		return cas.Object{}, err
	}
	return p.store.Put(ctx, d.MediaType, io.NewSectionReader(file, 0, d.SizeBytes))
}
func TestWorkerPreparationPublishesEncryptedDiskThroughHTTP(t *testing.T) {
	f, request := preparationTransportFixture(t)
	store := newTestUploadStore(t)
	wrapper, err := computerkey.NewLocal("preparation-publication", bytes.Repeat([]byte{19}, 32))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newPostgresServer(t, f.Pool, func(cfg *ServerConfig) { cfg.ComputerKeys = wrapper; cfg.CAS = store }))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	key, err := client.PreparationWriteKey(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Key)
	if _, err := client.PreparationSecrets(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if err := client.BeginPreparationCapture(t.Context(), workerapi.PreparationCaptureBegin{Executor: request, LogicalBytes: disk.SeedCapacity}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PreparationSecrets(t.Context(), request); err == nil {
		t.Fatal("capture did not seal exposure")
	}
	file, err := os.CreateTemp(t.TempDir(), "disk")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Truncate(disk.SeedCapacity); err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte{29}, 4096)
	if _, err := file.WriteAt(content, 4096); err != nil {
		t.Fatal(err)
	}
	candidate, err := disk.CaptureInitialVersion(t.Context(), disk.VersionCapture{Disk: file, Capacity: disk.SeedCapacity, StagingParent: t.TempDir(), Scope: key.Scope, KeyID: key.ID, Key: key.Key, Fanout: 64, PackLimit: blockformat.MinPackLimit, MaxStagedBytes: 4 << 20, MaxObjects: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	locator, err := candidate.Publish(t.Context(), preparationHTTPPublication{client: client, executor: request, store: store})
	if err != nil {
		t.Fatal(err)
	}
	root, err := disk.NewVersionRoot(locator, disk.SeedCapacity)
	if err != nil {
		t.Fatal(err)
	}
	receipt := workerapi.PreparationPublication{Executor: request, Root: root, Evidence: "host completed full disk capture"}
	if err := client.PublishPreparation(t.Context(), receipt); err == nil {
		t.Fatal("publication accepted without capture receipt")
	}
	if err := client.RecordPreparationCapture(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	wrong := receipt
	wrong.Executor.Identity.InstanceID = uuid.NewV7().String()
	if err := client.PublishPreparation(t.Context(), wrong); err == nil {
		t.Fatal("foreign instance published")
	}
	if err := client.PublishPreparation(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	tree, err := blockformat.OpenTree(t.Context(), store, key.Scope, map[string][]byte{key.ID: key.Key}, locator)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := tree.ReadBlock(t.Context(), 1)
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("published bytes: %v", err)
	}
	var succeeded, unfenced bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='succeeded',fenced_at IS NULL FROM computer_preparations WHERE environment_id=$1 AND id=$2`, f.Environment, request.Identity.OwnerID).Scan(&succeeded, &unfenced); err != nil || !succeeded || !unfenced {
		t.Fatalf("publication lost physical custody: %v", err)
	}
	if err := client.ObservePreparationStopped(t.Context(), request.Identity); err != nil {
		t.Fatal(err)
	}
	if err := client.PublishPreparation(t.Context(), receipt); err != nil {
		t.Fatalf("lost publication reply could not reconcile after stop: %v", err)
	}
	var images int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_images WHERE environment_id=$1 AND preparation_id=$2`, f.Environment, request.Identity.OwnerID).Scan(&images); err != nil || images != 1 {
		t.Fatalf("duplicate publication: %d %v", images, err)
	}
}

func TestWorkerPreparationStartBindsProgramAndRechecksStorageRead(t *testing.T) {
	for _, mode := range []string{"current", "changed Program", "changed bytes", "expiry during read", "seal during read"} {
		t.Run(mode, func(t *testing.T) {
			f, request := preparationTransportFixture(t)
			raw, manifest := controlPlaneDeploymentBundle(t)
			digest, err := bundle.Digest(raw)
			if err != nil {
				t.Fatal(err)
			}
			specs, err := artifact.BuildComputerPreparationSpecs(manifest.Program)
			if err != nil || len(specs) != 1 {
				t.Fatalf("specs: %v", err)
			}
			spec := specs[0]
			if mode == "changed Program" {
				spec.Program.Digest = "sha256:" + strings.Repeat("0", 64)
			}
			specRaw, err := artifact.CanonicalComputerPreparationSpec(spec)
			if err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_preparation_specs SET spec=$3 WHERE environment_id=$1 AND id=$2`, f.Environment, f.Deployment, string(specRaw))
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployments SET bundle_digest=$3,execution_revoked_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, f.Environment, f.Deployment, digest)
			store := &sessionBundleStore{bundleUploadStoreFixture: &bundleUploadStoreFixture{objects: map[string]cas.Object{digest: {Digest: digest, SizeBytes: int64(len(raw)), MediaType: bundle.MediaType}}}, body: raw}
			switch mode {
			case "changed bytes":
				store.body = bytes.Repeat([]byte("!"), len(raw))
			case "expiry during read":
				store.onRead = func() {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_preparations SET executor_expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.Environment, request.Identity.OwnerID)
				}
			case "seal during read":
				store.onRead = func() {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_preparations SET logical_bytes=$3 WHERE environment_id=$1 AND id=$2`, f.Environment, request.Identity.OwnerID, disk.SeedCapacity)
				}
			}
			server := httptest.NewServer(newPostgresServer(t, f.Pool, func(cfg *ServerConfig) { cfg.CAS = store }))
			defer server.Close()
			client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
			start, err := client.PreparationStart(t.Context(), request)
			if mode == "current" {
				if err != nil || start.Program.Artifact.Digest != manifest.Program.Artifact.Digest || start.Program.Runtime.Digest != manifest.Runtime.Artifact.Digest || start.ComputerDefinitionID != spec.ComputerDefinitionID || start.Seed.ArtifactDigest != manifest.ComputerSeeds[0].Artifact.Digest {
					t.Fatalf("executable preparation input: %+v %v", start, err)
				}
			} else if err == nil || start.Program.Artifact.Digest != "" {
				t.Fatalf("invalid executable input escaped: %v", err)
			}
		})
	}
}
