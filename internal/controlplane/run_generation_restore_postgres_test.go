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
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

type runHTTPPublication struct {
	client  *workerclient.Client
	store   *cas.File
	request workerapi.RunComputerObjectRequest
}

func (p runHTTPPublication) Register(ctx context.Context, e blockformat.ObjectInspection) error {
	r := p.request
	r.Inspection = e
	return p.client.RegisterRunComputerObject(ctx, r)
}
func (p runHTTPPublication) Certify(ctx context.Context, e blockformat.ObjectInspection) error {
	r := p.request
	r.Inspection = e
	return p.client.CertifyRunComputerObject(ctx, r)
}
func (p runHTTPPublication) Reuse(ctx context.Context, e blockformat.ObjectInspection) error {
	r := p.request
	r.Inspection = e
	return p.client.ReuseRunComputerObject(ctx, r)
}
func (p runHTTPPublication) Upload(ctx context.Context, d cas.Descriptor, f *os.File) (cas.Object, error) {
	return initialTestObjectPublisher{p.store}.Publish(ctx, d, f)
}

// Runs the production continuation HTTP protocol and completion transaction with
// real encrypted bytes. VM exclusion is represented by closing the local owner;
// physical VMM stop/restore remains a separate environment acceptance gate.
func TestRunGenerationPublicationRestoresAfterSourceRemoval(t *testing.T) {
	f, completion := finalizingActorRequest(t)
	base := retainedTestGeneration(t, f.Pool, f.server, pgvalue.UUIDString(f.claim.runtime.ID), computerPublicationKey("finalization", f.claim.runLease.ID, pgvalue.UUID(uuid.MustParse(completion.Workspace.Captured.Receipt.OperationID))))
	remote := f.server.cas.(*cas.File)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), workerContextKey{}, f.worker))
		switch r.URL.Path {
		case "/worker/v1/instance/token":
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{Token: "fixture", ExpiresInSeconds: 3600})
		case "/worker/v1/run/computer-objects/register":
			f.server.workerRegisterRunComputerObject(w, r)
		case "/worker/v1/run/computer-objects/certify":
			f.server.workerCertifyRunComputerObject(w, r)
		case "/worker/v1/run/computer-objects/reuse":
			f.server.workerReuseRunComputerObject(w, r)
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
	held, err := local.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	root := held.Root()
	completion.Workspace.Captured.Disk.Root = root
	request := workerapi.RegisterRunFinalizationRequest{Lease: completion.Lease, OperationID: completion.Workspace.Captured.Receipt.OperationID, Disk: completion.Workspace.Captured.Disk}
	if err := f.server.registerRunFinalization(t.Context(), f.worker, request); err != nil {
		t.Fatal(err)
	}
	publisher := runHTTPPublication{client, remote, workerapi.RunComputerObjectRequest{Lease: completion.Lease, OperationID: request.OperationID}}
	for range 2 {
		if err := held.Publish(t.Context(), publisher); err != nil {
			t.Fatal(err)
		}
	}
	parsed, err := parseActorCompletionRequest(completion)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.server.completeActor(t.Context(), f.worker, completion, parsed); err != nil {
		t.Fatal(err)
	}
	head := assertRetainedActorCapture(t, f, completion.Workspace.Captured, f.rootID)
	raw, err := f.server.db.GetComputerVersionRoot(t.Context(), db.GetComputerVersionRootParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: pgvalue.UUID(f.workspaceID), VersionID: pgvalue.UUID(head)})
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

func TestGenerationCheckpointClaimProjectsVersionWithoutArtifact(t *testing.T) {
	f := newActorCheckpointFixture(t)
	platform, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	object, err := platform.Put(t.Context(), deployment.RuntimeArtifactMediaType, bytes.NewReader([]byte("runtime descriptor fixture")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Pool.Exec(t.Context(), `UPDATE deployments SET runtime_artifact_digest=$2 WHERE id=$1`, f.DeploymentID, object.Digest); err != nil {
		t.Fatal(err)
	}
	f.server.platformStore = platform

	first := f.capture(t, "paired-computer")
	f.turn(t, 1)
	checkpoint := f.suspend(t, first)
	f.close(t)
	f.placeAndClaim(t)
	var response workerapi.RunLeaseClaimResponse
	f.workerCall(t, f.server.workerClaimRunLease, workerapi.RunLeaseClaimRequest{LeaseID: f.fence().ID, LeaseSequence: f.fence().LeaseSequence}, &response)
	if response.Workspace.Target.BaseWorkspaceVersionID != checkpoint.WorkspaceVersionID {
		t.Fatalf("target=%+v", response.Workspace.Target)
	}
}
