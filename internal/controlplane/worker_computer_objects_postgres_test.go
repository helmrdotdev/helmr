package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computerhost"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type objectStatObserver struct {
	cas.UploadStore
	after func()
	calls int
}

func (s *objectStatObserver) Stat(ctx context.Context, digest string) (cas.Object, error) {
	s.calls++
	o, e := s.UploadStore.Stat(ctx, digest)
	if s.after != nil {
		s.after()
	}
	return o, e
}

func TestInitialComputerObjectAuthenticatedPublication(t *testing.T) {
	f := newInitialPublicationFixture(t)
	remote := f.store
	observed := &objectStatObserver{UploadStore: remote}
	router := f.serve(t, func(cfg *ServerConfig) { cfg.CAS = observed })
	server := httptest.NewServer(router)
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.worker.HostID).client(t, server.URL)
	key, err := client.InitialComputerKey(t.Context(), workerapi.InitialComputerKeyRequest{ComputerInstanceID: pgvalue.UUIDString(f.instance), DesiredVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Key)
	local, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writer := blockformat.Writer{Source: local, Sink: local, Scope: key.Scope, ActiveKey: key.ID, Keys: map[string][]byte{key.ID: key.Key}, PackLimit: blockformat.MinPackLimit}
	candidate := func() workerapi.InitialComputerObjectRequest {
		t.Helper()
		root, err := writer.Empty(t.Context(), f.logicalBytes, 64)
		if err != nil {
			t.Fatal(err)
		}
		evidence, err := blockformat.InspectPack(t.Context(), local, key.Scope, writer.Keys, root.Pack)
		if err != nil {
			t.Fatal(err)
		}
		return workerapi.InitialComputerObjectRequest{ComputerInstanceID: pgvalue.UUIDString(f.instance), DesiredVersion: 1, Inspection: blockformat.ObjectInspection{Pack: &evidence}}
	}
	upload := func(r workerapi.InitialComputerObjectRequest) {
		t.Helper()
		digest := inspectedPackDigest(r.Inspection)
		body, err := local.Get(t.Context(), digest)
		if err != nil {
			t.Fatal(err)
		}
		defer body.Close()
		stored, err := remote.Put(t.Context(), "application/octet-stream", body)
		if err != nil || stored.Digest != digest {
			t.Fatalf("upload: %v", err)
		}
	}
	request := candidate()
	if err = client.CertifyInitialComputerObject(t.Context(), request); err == nil || observed.calls != 0 {
		t.Fatal("unregistered request probed storage")
	}
	payload, _ := json.Marshal(request)
	for _, suffix := range []string{"register", "certify"} {
		r := httptest.NewRequest("POST", "/worker/v1/run/computer-instances/initialization/objects/"+suffix, bytes.NewReader(payload))
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
	wrong.ComputerInstanceID = uuid.NewV7().String()
	before := observed.calls
	if err = client.CertifyInitialComputerObject(t.Context(), wrong); err == nil || observed.calls != before {
		t.Fatal("foreign Runtime probed storage")
	}
	// Exercise the actual bounded producer and execution adapter through these
	// authenticated routes, using the admitted disk geometry and sparse contents.
	diskFile, err := os.CreateTemp(t.TempDir(), "disk")
	if err != nil {
		t.Fatal(err)
	}
	defer diskFile.Close()
	if err = diskFile.Truncate(f.logicalBytes); err != nil {
		t.Fatal(err)
	}
	if _, err = diskFile.WriteAt(bytes.Repeat([]byte{9}, 4096), (4<<20)+4096); err != nil {
		t.Fatal(err)
	}
	initial, err := disk.CaptureInitialVersion(t.Context(), disk.VersionCapture{Disk: diskFile, Capacity: f.logicalBytes, StagingParent: t.TempDir(), Scope: key.Scope, KeyID: key.ID, Key: key.Key, Fanout: 64, PackLimit: blockformat.MinPackLimit, MaxStagedBytes: 32 << 20, MaxObjects: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer initial.Close()
	publication, err := computerhost.NewInitialVersionPublisher(client, initialTestObjectPublisher{remote}, request.ComputerInstanceID, request.DesiredVersion)
	if err != nil {
		t.Fatal(err)
	}
	versionRoot, err := initial.Publish(t.Context(), publication)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := blockformat.OpenTree(t.Context(), remote, key.Scope, writer.Keys, versionRoot)
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
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.instance)
	}
	if err = client.CertifyInitialComputerObject(t.Context(), next); err == nil {
		t.Fatal("revoked Runtime certified after storage I/O")
	}
	nextDigest := inspectedPackDigest(next.Inspection)
	var recorded int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM cas_objects WHERE digest=$1`, nextDigest).Scan(&recorded); err != nil || recorded != 0 {
		t.Fatalf("revoked operation leaked membership: %d %v", recorded, err)
	}
	q := db.New(f.Pool)
	if n, err := q.ReleaseReclaimedComputerObjects(t.Context(), 100); err != nil || n != 0 {
		t.Fatalf("close request released candidates: %d %v", n, err)
	}
	if _, err = f.Pool.Exec(t.Context(), `DELETE FROM computer_objects WHERE digest=$1`, nextDigest); err == nil {
		t.Fatal("live candidate collected")
	}
	var version int64
	if err = f.Pool.QueryRow(t.Context(), `SELECT observed_version FROM computer_instances WHERE id=$1`, f.instance).Scan(&version); err != nil {
		t.Fatal(err)
	}
	closure := computer.Closure{
		Observation: computer.Observation{
			Instance: computer.InstanceRef{
				Host: computer.Host{GroupID: f.worker.GroupID, HostID: f.worker.HostID, Epoch: f.worker.Epoch},
				ID:   pgvalue.MustUUIDValue(f.instance), DesiredVersion: 2,
			},
			ExpectedObservedVersion: version,
		},
		Reason: "test_cleanup", CleanupProof: &computer.CleanupProof{Method: computer.CleanupMachineClosed, CompletedAt: time.Now()},
	}
	if _, err = computer.RecordInstanceClosed(t.Context(), f.Pool, closure); err != nil {
		t.Fatal(err)
	}
	var expectedRetained int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_object_pins WHERE computer_instance_id=$1`, f.instance).Scan(&expectedRetained); err != nil {
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

type initialTestObjectPublisher struct{ store cas.Store }

func (p initialTestObjectPublisher) Publish(ctx context.Context, d cas.Descriptor, file *os.File) (cas.Object, error) {
	if err := cas.VerifyDescriptorFile(ctx, d, file); err != nil {
		return cas.Object{}, err
	}
	return p.store.Put(ctx, d.MediaType, io.NewSectionReader(file, 0, d.SizeBytes))
}
