package executor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/runtime"
)

func TestPreparedRuntimeRestoreRebuildsAndRegistersSubstrateWithoutSubstrateCAS(t *testing.T) {
	store, fixture := testWorkspaceMountArtifacts(t)
	target := api.WorkerRuntimeReconcileTarget{
		Source: api.WorkerRuntimeSource{
			WorkspaceID:            "019c10d5-a6f7-7af1-8f5f-000000000801",
			DeploymentDefinitionID: "019c10d5-a6f7-7af1-8f5f-000000000802",
			BaseVersionID:          "019c10d5-a6f7-7af1-8f5f-000000000803",
			WorkspaceImage:         fixture.WorkspaceImage,
			Restore:                &api.WorkerRuntimeRestore{CheckpointID: "checkpoint-1"},
		},
	}
	substratePath := t.TempDir() + "/substrate.ext4"
	if err := os.WriteFile(substratePath, []byte("rebuilt substrate"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver := &restoreSubstrateResolver{
		result: runtime.Result{
			Path:       substratePath,
			Digest:     "sha256:" + strings.Repeat("a", 64),
			Format:     runtime.Format,
			BuilderABI: runtime.BuilderABI,
			LayoutABI:  runtime.LayoutABI,
			SizeBytes:  int64(len("rebuilt substrate")),
		},
	}
	pool := &PreparedRuntimePool{Substrates: resolver}
	mount := preparedRuntimeWorkspaceMountFromSource(target.Source)
	_, cleanup, topology, err := pool.restoreWorkspaceImageAndRuntimeSubstrate(
		context.Background(),
		WorkspaceMaterializer{CAS: store},
		t.TempDir(),
		mount,
		"restore image",
		"restore substrate",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if resolver.calls != 1 ||
		resolver.source.WorkspaceImageDigest != target.Source.WorkspaceImage.Digest ||
		resolver.source.WorkspaceImageMediaType != target.Source.WorkspaceImage.MediaType {
		t.Fatalf("resolver calls = %d, source = %+v", resolver.calls, resolver.source)
	}
	if topology.Substrate == nil ||
		topology.Substrate.Digest != resolver.result.Digest ||
		topology.Substrate.SizeBytes != resolver.result.SizeBytes {
		t.Fatalf("topology = %+v, want rebuilt substrate", topology)
	}

	registrar := &immutableSubstrateRegistrar{}
	registered, err := registerRuntimeSubstrate(
		context.Background(),
		registrar,
		target.Source.DeploymentDefinitionID,
		topology.Substrate,
	)
	if err != nil {
		t.Fatal(err)
	}
	if registered.ID != registrar.id ||
		registrar.request.SubstrateDigest != resolver.result.Digest ||
		registrar.request.Format != runtime.Format ||
		registrar.request.BuilderABI != runtime.BuilderABI ||
		registrar.request.LayoutABI != runtime.LayoutABI ||
		registrar.request.SizeBytes != resolver.result.SizeBytes {
		t.Fatalf("registration = %+v, request = %+v", registered, registrar.request)
	}
	if got := store.getCalls[target.Source.WorkspaceImage.Digest]; got != 1 {
		t.Fatalf("Workspace image CAS reads = %d, want 1", got)
	}
	if len(store.getCalls) != 1 {
		t.Fatalf("CAS reads = %+v, want only the Workspace image", store.getCalls)
	}

	conflicting := *topology.Substrate
	conflicting.Digest = "sha256:" + strings.Repeat("b", 64)
	if _, err := registerRuntimeSubstrate(
		context.Background(),
		registrar,
		target.Source.DeploymentDefinitionID,
		&conflicting,
	); !errors.Is(err, errRuntimeSubstrateConflict) {
		t.Fatalf("conflicting registration error = %v, want conflict", err)
	}
}

type restoreSubstrateResolver struct {
	calls  int
	source runtime.Source
	result runtime.Result
}

func (r *restoreSubstrateResolver) Resolve(
	_ context.Context,
	imagePath string,
	source runtime.Source,
) (runtime.Result, error) {
	image, err := os.ReadFile(imagePath)
	if err != nil {
		return runtime.Result{}, err
	}
	if string(image) != "oci image" {
		return runtime.Result{}, errors.New("unexpected Workspace image")
	}
	r.calls++
	r.source = source
	return r.result, nil
}

var errRuntimeSubstrateConflict = errors.New("runtime substrate conflict")

type immutableSubstrateRegistrar struct {
	id      string
	request api.WorkerRuntimeSubstrateRegisterRequest
}

func (r *immutableSubstrateRegistrar) RegisterRuntimeSubstrate(
	_ context.Context,
	request api.WorkerRuntimeSubstrateRegisterRequest,
) (api.WorkerRuntimeSubstrateRegisterResponse, error) {
	if r.id == "" {
		r.id = "019c10d5-a6f7-7af1-8f5f-000000000804"
		r.request = request
	} else if request.DeploymentDefinitionID != r.request.DeploymentDefinitionID ||
		request.Format != r.request.Format ||
		request.BuilderABI != r.request.BuilderABI ||
		request.LayoutABI != r.request.LayoutABI ||
		request.SubstrateDigest != r.request.SubstrateDigest ||
		request.SizeBytes != r.request.SizeBytes {
		return api.WorkerRuntimeSubstrateRegisterResponse{}, errRuntimeSubstrateConflict
	}
	return api.WorkerRuntimeSubstrateRegisterResponse{
		RuntimeSubstrate: api.WorkerRuntimeSubstrate{
			ID:                     r.id,
			DeploymentDefinitionID: request.DeploymentDefinitionID,
			SubstrateDigest:        request.SubstrateDigest,
			Format:                 request.Format,
			BuilderABI:             request.BuilderABI,
			LayoutABI:              request.LayoutABI,
			SizeBytes:              request.SizeBytes,
		},
	}, nil
}

var _ RuntimeSubstrateResolver = (*restoreSubstrateResolver)(nil)
var _ RuntimeSubstrateRegistrar = (*immutableSubstrateRegistrar)(nil)
