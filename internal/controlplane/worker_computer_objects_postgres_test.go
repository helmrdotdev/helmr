package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/executor"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"github.com/jackc/pgx/v5/pgtype"
)

type objectStatObserver struct {
	cas.Store
	after func()
	calls int
}

func (s *objectStatObserver) Stat(ctx context.Context, digest string) (cas.Object, error) {
	s.calls++
	o, e := s.Store.Stat(ctx, digest)
	if s.after != nil {
		s.after()
	}
	return o, e
}

func TestInitialComputerObjectAuthenticatedPublication(t *testing.T) {
	f, broker, fence := initialKeyFixture(t)
	credentialID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_instance_credentials(id,worker_group_id,worker_instance_id,key_prefix,secret_hash,claim_version) VALUES($1,$2,$3,'object-test-prefix',$4,$5)`, credentialID, fence.WorkerGroupID, fence.WorkerID, []byte("test-hash"), fence.ClaimVersion)
	signingKey := bytes.Repeat([]byte{0x49}, 32)
	claims := auth.WorkerClaims{WorkerGroupID: pgvalue.UUIDString(fence.WorkerGroupID), WorkerInstanceID: pgvalue.UUIDString(fence.WorkerID), CredentialID: credentialID.String(), WorkerEpoch: fence.WorkerEpoch, ClaimVersion: fence.ClaimVersion, GroupClaimVersion: fence.GroupClaimVersion, IssuedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour)}
	token, err := auth.IssueWorkerToken(signingKey, claims)
	if err != nil {
		t.Fatal(err)
	}
	f.server.computerKeys = broker
	f.server.workerTokenSigningKey = signingKey
	f.server.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	remote, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	observed := &objectStatObserver{Store: remote}
	f.server.cas = observed
	router := chi.NewRouter()
	f.server.mountWorkerRoutes(router)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/instance/token" {
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{Token: token, ExpiresInSeconds: 3600})
			return
		}
		router.ServeHTTP(w, r)
	}))
	defer server.Close()
	client, err := workerclient.New(server.URL, workerclient.WithAuth(claims.WorkerInstanceID, "fixture-secret"), workerclient.WithService(uuid.NewV7().String()))
	if err != nil {
		t.Fatal(err)
	}
	key, err := client.InitialComputerKey(t.Context(), workerapi.InitialComputerKeyRequest{RuntimeInstanceID: pgvalue.UUIDString(fence.RuntimeID), DesiredVersion: fence.DesiredVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Key)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := dispatch.LockComputerPreparation(t.Context(), tx, fence.ComputerPreparationFence)
	_ = tx.Rollback(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	local, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writer := blockformat.Writer{Source: local, Sink: local, Scope: key.Scope, ActiveKey: key.ID, Keys: map[string][]byte{key.ID: key.Key}, PackLimit: blockformat.MinPackLimit}
	candidate := func() workerapi.InitialComputerObjectRequest {
		t.Helper()
		root, err := writer.Empty(t.Context(), owner.LogicalBytes, 64)
		if err != nil {
			t.Fatal(err)
		}
		evidence, err := blockformat.InspectPack(t.Context(), local, key.Scope, writer.Keys, root.Pack)
		if err != nil {
			t.Fatal(err)
		}
		return workerapi.InitialComputerObjectRequest{RuntimeInstanceID: pgvalue.UUIDString(fence.RuntimeID), DesiredVersion: fence.DesiredVersion, Inspection: blockformat.ObjectInspection{Pack: &evidence}}
	}
	upload := func(r workerapi.InitialComputerObjectRequest) {
		t.Helper()
		o, err := describeComputerObject(r.Inspection)
		if err != nil {
			t.Fatal(err)
		}
		body, err := local.Get(t.Context(), o.digest)
		if err != nil {
			t.Fatal(err)
		}
		defer body.Close()
		stored, err := remote.Put(t.Context(), "application/octet-stream", body)
		if err != nil || stored.Digest != o.digest {
			t.Fatalf("upload: %v", err)
		}
	}
	request := candidate()
	if err = client.CertifyInitialComputerObject(t.Context(), request); err == nil || observed.calls != 0 {
		t.Fatal("unregistered request probed storage")
	}
	payload, _ := json.Marshal(request)
	for _, suffix := range []string{"register", "certify"} {
		r := httptest.NewRequest("POST", "/worker/v1/run/runtime-instances/initialization/objects/"+suffix, bytes.NewReader(payload))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("missing auth: %d", w.Code)
		}
	}
	if err = client.RegisterInitialComputerObject(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if err = client.CertifyInitialComputerObject(t.Context(), request); err == nil {
		t.Fatal("missing remote bytes certified")
	}
	upload(request)
	for range 2 {
		if err = client.CertifyInitialComputerObject(t.Context(), request); err != nil {
			t.Fatal(err)
		}
	}
	var certified int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_objects WHERE certified`).Scan(&certified); err != nil || certified != 1 {
		t.Fatalf("certification: %d %v", certified, err)
	}
	wrong := request
	wrong.RuntimeInstanceID = uuid.NewV7().String()
	before := observed.calls
	if err = client.CertifyInitialComputerObject(t.Context(), wrong); err == nil || observed.calls != before {
		t.Fatal("foreign Runtime probed storage")
	}
	// Exercise the actual bounded producer and execution adapter through these
	// authenticated routes, using the admitted disk geometry and sparse contents.
	disk, err := os.CreateTemp(t.TempDir(), "disk")
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	if err = disk.Truncate(owner.LogicalBytes); err != nil {
		t.Fatal(err)
	}
	if _, err = disk.WriteAt(bytes.Repeat([]byte{9}, 4096), (4<<20)+4096); err != nil {
		t.Fatal(err)
	}
	generation, err := computer.CaptureInitialGeneration(t.Context(), computer.GenerationCapture{Disk: disk, Capacity: owner.LogicalBytes, StagingParent: t.TempDir(), Scope: key.Scope, KeyID: key.ID, Key: key.Key, Fanout: 64, PackLimit: blockformat.MinPackLimit, MaxStagedBytes: 32 << 20, MaxObjects: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer generation.Close()
	publication, err := executor.NewInitialGenerationPublisher(client, initialTestObjectPublisher{remote}, request.RuntimeInstanceID, request.DesiredVersion)
	if err != nil {
		t.Fatal(err)
	}
	generationRoot, err := generation.Publish(t.Context(), publication)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := blockformat.OpenTree(t.Context(), remote, key.Scope, writer.Keys, generationRoot)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := tree.ReadBlock(t.Context(), 1025)
	if err != nil || !bytes.Equal(restored, bytes.Repeat([]byte{9}, 4096)) {
		t.Fatalf("published disk read: %v", err)
	}
	// Storage success cannot authorize publication after an intervening fence.
	next := candidate()
	if err = client.RegisterInitialComputerObject(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	upload(next)
	observed.after = func() {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtime)
	}
	if err = client.CertifyInitialComputerObject(t.Context(), next); err == nil {
		t.Fatal("revoked Runtime certified after storage I/O")
	}
	o, _ := describeComputerObject(next.Inspection)
	var recorded int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM cas_objects WHERE digest=$1`, o.digest).Scan(&recorded); err != nil || recorded != 0 {
		t.Fatalf("revoked operation leaked membership: %d %v", recorded, err)
	}
	q := db.New(f.Pool)
	if n, err := q.ReleaseReclaimedComputerObjects(t.Context(), 100); err != nil || n != 0 {
		t.Fatalf("close request released candidates: %d %v", n, err)
	}
	if _, err = f.Pool.Exec(t.Context(), `DELETE FROM computer_objects WHERE digest=$1`, o.digest); err == nil {
		t.Fatal("live candidate collected")
	}
	var version int64
	if err = f.Pool.QueryRow(t.Context(), `SELECT observed_version FROM runtime_instances WHERE id=$1`, f.runtime).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err = q.MarkRuntimeInstanceClosed(t.Context(), db.MarkRuntimeInstanceClosedParams{ID: f.runtime, WorkerInstanceID: fence.WorkerID, WorkerEpoch: fence.WorkerEpoch, DesiredVersion: fence.DesiredVersion + 1, ExpectedObservedVersion: version, ReasonCode: pgtype.Text{String: "test_cleanup", Valid: true}, CleanupProof: []byte(`{"method":"session_closed"}`)}); err != nil {
		t.Fatal(err)
	}
	var expectedRetained int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM runtime_computer_object_pins WHERE runtime_instance_id=$1`, f.runtime).Scan(&expectedRetained); err != nil {
		t.Fatal(err)
	}
	for range expectedRetained {
		if n, err := q.ReleaseReclaimedComputerObjects(t.Context(), 1); err != nil || n != 1 {
			t.Fatalf("bounded candidate release: %d %v", n, err)
		}
	}
	if n, err := q.ReleaseReclaimedComputerObjects(t.Context(), 100); err != nil || n != 0 {
		t.Fatalf("release retry: %d %v", n, err)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_objects`).Scan(&recorded); err != nil || recorded != expectedRetained {
		t.Fatalf("candidate release deleted objects: %d %v", recorded, err)
	}

}

type initialTestObjectPublisher struct{ store *cas.File }

func (p initialTestObjectPublisher) Publish(ctx context.Context, d cas.Descriptor, file *os.File) (cas.Object, error) {
	if err := cas.VerifyDescriptorFile(ctx, d, file); err != nil {
		return cas.Object{}, err
	}
	return p.store.Put(ctx, d.MediaType, io.NewSectionReader(file, 0, d.SizeBytes))
}
