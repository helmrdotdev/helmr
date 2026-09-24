//go:build linux || darwin

package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"time"
	"uuid"
)

type execHTTPPublication struct {
	client  *workerclient.Client
	store   *cas.File
	request workerapi.ExecComputerObjectRequest
}

func (p execHTTPPublication) Register(ctx context.Context, e blockformat.ObjectInspection) error {
	r := p.request
	r.Inspection = e
	return p.client.RegisterExecComputerObject(ctx, r)
}
func (p execHTTPPublication) Certify(ctx context.Context, e blockformat.ObjectInspection) error {
	r := p.request
	r.Inspection = e
	return p.client.CertifyExecComputerObject(ctx, r)
}
func (p execHTTPPublication) Reuse(ctx context.Context, e blockformat.ObjectInspection) error {
	r := p.request
	r.Inspection = e
	return p.client.ReuseExecComputerObject(ctx, r)
}
func (p execHTTPPublication) Upload(ctx context.Context, d cas.Descriptor, f *os.File) (cas.Object, error) {
	return initialTestObjectPublisher{p.store}.Publish(ctx, d, f)
}

// Runs the production continuation HTTP protocol and completion transaction with
// real encrypted bytes. VM exclusion is represented by closing the local owner;
// physical VMM stop/restore remains a separate environment acceptance gate.
func TestExecGenerationPublicationRestoresAfterSourceRemoval(t *testing.T) {
	f := newExecGenerationFixture(t)
	base := f.root
	remote := f.server.cas.(*cas.File)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), workerContextKey{}, f.worker))
		switch r.URL.Path {
		case "/worker/v1/instance/token":
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{Token: "fixture", ExpiresInSeconds: 3600})
		case "/worker/v1/run/workspace-mounts/computer-objects/register":
			f.server.workerRegisterExecComputerObject(w, r)
		case "/worker/v1/run/workspace-mounts/computer-objects/certify":
			f.server.workerCertifyExecComputerObject(w, r)
		case "/worker/v1/run/workspace-mounts/computer-objects/reuse":
			f.server.workerReuseExecComputerObject(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := workerclient.New(server.URL, workerclient.WithAuth(f.worker.WorkerInstanceID.String(), "fixture"), workerclient.WithService("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	key[0] = 42
	cfg := computer.LocalGenerationConfig{Directory: filepath.Join(t.TempDir(), "runtime-one"), Base: base, BaseSource: remote, Scope: "fixture", ActiveKey: base.Page.KeyID, Keys: map[string][]byte{base.Page.KeyID: key}, DirtyBlocks: 8, StagedBytes: 32 << 20, PackLimit: blockformat.MinPackLimit}
	local, err := computer.CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	want := []byte("completed work survives source Runtime removal")
	if _, err := local.WriteAt(t.Context(), want, 4096); err != nil {
		t.Fatal(err)
	}
	root, err := local.Flush(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	publisher := execHTTPPublication{client, remote, workerapi.ExecComputerObjectRequest{OrgID: f.OrgID.String(), WorkspaceMountID: f.mountID.String()}}
	// Initial preparation's retained objects survive a desired-state transition.
	if _, err := f.Pool.Exec(t.Context(), `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtimeID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := local.Publish(t.Context(), root, publisher, 1000); err != nil {
			t.Fatal(err)
		}
	}
	capture := f.capture()
	capture.Computer.Root = root
	if w := f.call(t, f.server.workerCaptureWorkspaceMount, capture); w.Code != 200 {
		t.Fatalf("capture %d %s", w.Code, w.Body)
	}
	stop := workerapi.WorkspaceMountStopRequest{OrgID: f.OrgID.String(), WorkspaceMountID: f.mountID.String(), CleanupProof: workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now().UTC()}}
	if w := f.call(t, f.server.workerStopWorkspaceMount, stop); w.Code != 200 {
		t.Fatalf("stop %d %s", w.Code, w.Body)
	}
	var head string
	if err := f.Pool.QueryRow(t.Context(), `SELECT head_version_id::text FROM computers WHERE id=$1`, f.computerID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	raw, err := f.server.db.GetComputerVersionRoot(t.Context(), db.GetComputerVersionRootParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: pgvalue.UUID(f.computerID), VersionID: pgvalue.UUID(uuid.MustParse(head))})
	if err != nil {
		t.Fatal(err)
	}
	committed, err := computer.ParseGenerationRoot(raw, root.LogicalBytes)
	if err != nil || committed != root {
		t.Fatalf("committed root changed: %v", err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(cfg.Directory); err != nil {
		t.Fatal(err)
	}
	cfg.Directory = filepath.Join(t.TempDir(), "runtime-two")
	cfg.Base = committed
	next, err := computer.CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	got := make([]byte, len(want))
	if _, err := next.ReadAt(t.Context(), got, 4096); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("remote restore: %v", err)
	}
}
