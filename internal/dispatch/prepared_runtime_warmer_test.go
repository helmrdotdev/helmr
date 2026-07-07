package dispatch

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/substrate"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRuntimePreparerCreatesWarmCommandWithSource(t *testing.T) {
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	projectID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	environmentID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	workerID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	sandboxID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeRuntimePreparerStore{
		targets: []db.ListRuntimeInstanceWarmTargetsRow{{
			OrgID:                         orgID,
			ProjectID:                     projectID,
			EnvironmentID:                 environmentID,
			WorkerInstanceID:              workerID,
			DeploymentSandboxID:           sandboxID,
			RuntimeIdentityID:             "runtime-1",
			RootfsDigest:                  "sha256:rootfs",
			RuntimeABI:                    "runtime-abi",
			SandboxImageArtifactDigest:    "sha256:sandbox",
			SandboxImageArtifactMediaType: api.SandboxImageArtifactMediaType,
			SandboxImageArtifactSizeBytes: 123,
			SandboxImageArtifactFormat:    "oci-tar",
			ImageDigest:                   "sha256:image",
			ImageFormat:                   "oci-tar",
			WorkspaceMountPath:            "/workspace",
			GuestdAbi:                     "guestd-abi",
			AdapterAbi:                    "adapter-abi",
		}},
	}
	warmer, err := NewRuntimePreparer(store, WithRuntimePrepareTarget(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := warmer.WarmOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.lastSubstrateParams.SubstrateFormat != substrate.Format ||
		store.lastSubstrateParams.SubstrateBuilderAbi != substrate.BuilderABI ||
		store.lastSubstrateParams.SubstrateLayoutAbi != substrate.LayoutABI ||
		store.lastSubstrateParams.RowLimit != 20 {
		t.Fatalf("substrate prepare target params = %+v, want current substrate identity", store.lastSubstrateParams)
	}
	if len(store.commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(store.commands))
	}
	command := store.commands[0]
	if command.Kind != db.WorkerCommandKindRuntimePrepare {
		t.Fatalf("command kind = %q", command.Kind)
	}
	if command.RunID.Valid || command.RunWaitID.Valid || command.RunLeaseID.Valid {
		t.Fatalf("warm command should not have run scope: %+v", command)
	}
	var payload api.WorkerRuntimePrepareCommand
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.DeploymentSandboxID != pgvalue.MustUUIDValue(sandboxID).String() {
		t.Fatalf("deployment sandbox id = %q", payload.DeploymentSandboxID)
	}
	if payload.Source.RuntimeID != "runtime-1" || payload.Source.SandboxImageArtifact.Digest != "sha256:sandbox" || payload.Source.WorkspaceMountPath != "/workspace" {
		t.Fatalf("payload source = %+v", payload.Source)
	}
	if payload.RuntimeInstance.ID == "" || payload.RuntimeInstance.InstanceToken == "" || payload.RuntimeInstance.State != string(db.RuntimeInstanceStatePreparing) {
		t.Fatalf("payload runtime instance = %+v", payload.RuntimeInstance)
	}
	if len(store.createdRuntimeInstances) != 1 {
		t.Fatalf("created runtime instances = %d, want 1", len(store.createdRuntimeInstances))
	}
}

func TestRuntimePreparerTargetZeroDoesNothing(t *testing.T) {
	store := &fakeRuntimePreparerStore{}
	warmer, err := NewRuntimePreparer(store, WithRuntimePrepareTarget(0))
	if err != nil {
		t.Fatal(err)
	}
	if err := warmer.WarmOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.listCalls != 0 || len(store.commands) != 0 {
		t.Fatalf("listCalls=%d commands=%d, want no work", store.listCalls, len(store.commands))
	}
}

func TestRuntimePreparerReconcilesDeploymentSandbox(t *testing.T) {
	sandboxID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeRuntimePreparerStore{}
	warmer, err := NewRuntimePreparer(store, WithRuntimePrepareTarget(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := warmer.ReconcileDeploymentSandbox(context.Background(), sandboxID); err != nil {
		t.Fatal(err)
	}
	if !store.lastWarmTargetParams.DeploymentSandboxID.Valid {
		t.Fatal("deployment sandbox id was not passed to warm target query")
	}
	if store.lastWarmTargetParams.DeploymentSandboxID != sandboxID {
		t.Fatalf("deployment sandbox id = %+v, want %+v", store.lastWarmTargetParams.DeploymentSandboxID, sandboxID)
	}
}

func TestRuntimePreparerCreatesSubstratePrepareCommand(t *testing.T) {
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	projectID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	environmentID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	workerID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	sandboxID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeRuntimePreparerStore{
		substrateTargets: []db.ListRuntimeSubstratePrepareTargetsRow{{
			OrgID:                         orgID,
			ProjectID:                     projectID,
			EnvironmentID:                 environmentID,
			WorkerInstanceID:              workerID,
			DeploymentSandboxID:           sandboxID,
			RuntimeIdentityID:             "runtime-1",
			RootfsDigest:                  "sha256:rootfs",
			RuntimeABI:                    "runtime-abi",
			SandboxImageArtifactDigest:    "sha256:sandbox",
			SandboxImageArtifactMediaType: api.SandboxImageArtifactMediaType,
			SandboxImageArtifactSizeBytes: 123,
			SandboxImageArtifactFormat:    "oci-tar",
			ImageDigest:                   "sha256:image",
			ImageFormat:                   "oci-tar",
			WorkspaceMountPath:            "/workspace",
			GuestdAbi:                     "guestd-abi",
			AdapterAbi:                    "adapter-abi",
		}},
	}
	warmer, err := NewRuntimePreparer(store, WithRuntimePrepareTarget(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := warmer.WarmOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(store.commands))
	}
	command := store.commands[0]
	if command.Kind != db.WorkerCommandKindRuntimeSubstratePrepare {
		t.Fatalf("command kind = %q", command.Kind)
	}
	var payload api.WorkerRuntimeSubstratePrepareCommand
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.DeploymentSandboxID != pgvalue.MustUUIDValue(sandboxID).String() {
		t.Fatalf("deployment sandbox id = %q", payload.DeploymentSandboxID)
	}
	if payload.Source.SandboxImageArtifact.Digest != "sha256:sandbox" || payload.Source.RuntimeABI != "runtime-abi" {
		t.Fatalf("payload source = %+v", payload.Source)
	}
}

func TestValidateRuntimePreparerWorkerCommand(t *testing.T) {
	id := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	tests := []struct {
		name    string
		params  db.CreateWorkerCommandParams
		wantErr bool
	}{
		{
			name: "runtime prepare",
			params: db.CreateWorkerCommandParams{
				Kind:              db.WorkerCommandKindRuntimePrepare,
				RuntimeInstanceID: id,
				RuntimeEpoch:      pgtype.Int8{Int64: 1, Valid: true},
			},
		},
		{
			name: "runtime prepare rejects deployment sandbox",
			params: db.CreateWorkerCommandParams{
				Kind:                db.WorkerCommandKindRuntimePrepare,
				RuntimeInstanceID:   id,
				RuntimeEpoch:        pgtype.Int8{Int64: 1, Valid: true},
				DeploymentSandboxID: id,
			},
			wantErr: true,
		},
		{
			name: "runtime substrate prepare",
			params: db.CreateWorkerCommandParams{
				Kind:                db.WorkerCommandKindRuntimeSubstratePrepare,
				DeploymentSandboxID: id,
			},
		},
		{
			name: "runtime substrate prepare rejects runtime instance",
			params: db.CreateWorkerCommandParams{
				Kind:                db.WorkerCommandKindRuntimeSubstratePrepare,
				DeploymentSandboxID: id,
				RuntimeInstanceID:   id,
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRuntimePreparerWorkerCommand(tt.params)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRuntimePreparerMarksSupersededInstancesBeforeTargetSelection(t *testing.T) {
	store := &fakeRuntimePreparerStore{
		superseded: []db.WorkerCommand{{ID: 1, Kind: db.WorkerCommandKindRuntimeStop}},
	}
	warmer, err := NewRuntimePreparer(store, WithRuntimePrepareTarget(1), WithRuntimePrepareLimit(7))
	if err != nil {
		t.Fatal(err)
	}
	if err := warmer.WarmOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.supersededLimit != 7 {
		t.Fatalf("superseded limit = %d, want 7", store.supersededLimit)
	}
	wantOrder := []string{"release-expired", "mark-superseded", "list-targets", "list-substrates"}
	if len(store.callOrder) != len(wantOrder) {
		t.Fatalf("call order = %v, want %v", store.callOrder, wantOrder)
	}
	for i := range wantOrder {
		if store.callOrder[i] != wantOrder[i] {
			t.Fatalf("call order = %v, want %v", store.callOrder, wantOrder)
		}
	}
}

func TestRuntimePreparerReleasesExpiredReservationsBeforeTargetSelection(t *testing.T) {
	store := &fakeRuntimePreparerStore{
		released: []db.ReleaseExpiredPreparedRuntimeReservationsRow{{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		}},
	}
	warmer, err := NewRuntimePreparer(store, WithRuntimePrepareTarget(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := warmer.WarmOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.releaseExpiredBefore.Valid {
		t.Fatal("expired reservation cutoff was not passed")
	}
	wantOrder := []string{"release-expired", "mark-superseded", "list-targets", "list-substrates"}
	if len(store.callOrder) != len(wantOrder) {
		t.Fatalf("call order = %v, want %v", store.callOrder, wantOrder)
	}
	for i := range wantOrder {
		if store.callOrder[i] != wantOrder[i] {
			t.Fatalf("call order = %v, want %v", store.callOrder, wantOrder)
		}
	}
}

type fakeRuntimePreparerStore struct {
	targets                 []db.ListRuntimeInstanceWarmTargetsRow
	substrateTargets        []db.ListRuntimeSubstratePrepareTargetsRow
	superseded              []db.WorkerCommand
	released                []db.ReleaseExpiredPreparedRuntimeReservationsRow
	commands                []db.CreateWorkerCommandParams
	createdRuntimeInstances []db.CreateRuntimeInstanceForDeploymentSandboxParams
	listCalls               int
	lastWarmTargetParams    db.ListRuntimeInstanceWarmTargetsParams
	lastSubstrateParams     db.ListRuntimeSubstratePrepareTargetsParams
	supersededLimit         int32
	releaseExpiredBefore    pgtype.Timestamptz
	callOrder               []string
}

func (f *fakeRuntimePreparerStore) ReleaseExpiredPreparedRuntimeReservations(_ context.Context, expiredBefore pgtype.Timestamptz) ([]db.ReleaseExpiredPreparedRuntimeReservationsRow, error) {
	f.releaseExpiredBefore = expiredBefore
	f.callOrder = append(f.callOrder, "release-expired")
	return f.released, nil
}

func (f *fakeRuntimePreparerStore) CreateSupersededPreparedRuntimeStopCommands(_ context.Context, rowLimit int32) ([]db.WorkerCommand, error) {
	f.supersededLimit = rowLimit
	f.callOrder = append(f.callOrder, "mark-superseded")
	return f.superseded, nil
}

func (f *fakeRuntimePreparerStore) ListRuntimeInstanceWarmTargets(_ context.Context, arg db.ListRuntimeInstanceWarmTargetsParams) ([]db.ListRuntimeInstanceWarmTargetsRow, error) {
	f.listCalls++
	f.lastWarmTargetParams = arg
	f.callOrder = append(f.callOrder, "list-targets")
	return f.targets, nil
}

func (f *fakeRuntimePreparerStore) ListRuntimeSubstratePrepareTargets(_ context.Context, arg db.ListRuntimeSubstratePrepareTargetsParams) ([]db.ListRuntimeSubstratePrepareTargetsRow, error) {
	f.listCalls++
	f.lastSubstrateParams = arg
	f.callOrder = append(f.callOrder, "list-substrates")
	return f.substrateTargets, nil
}

func (f *fakeRuntimePreparerStore) CreateRuntimeInstanceForDeploymentSandbox(_ context.Context, arg db.CreateRuntimeInstanceForDeploymentSandboxParams) (db.CreateRuntimeInstanceForDeploymentSandboxRow, error) {
	f.createdRuntimeInstances = append(f.createdRuntimeInstances, arg)
	return db.CreateRuntimeInstanceForDeploymentSandboxRow{
		ID:                            arg.ID,
		OrgID:                         pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ProjectID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkerInstanceID:              arg.WorkerInstanceID,
		RuntimeIdentityID:             arg.RuntimeIdentityID,
		DeploymentSandboxID:           arg.DeploymentSandboxID,
		RuntimeKeyHash:                arg.RuntimeKeyHash,
		RuntimeKey:                    arg.RuntimeKey,
		SandboxFingerprint:            "sandbox-fingerprint",
		RootfsDigest:                  arg.RootfsDigest,
		ImageDigest:                   "sha256:image",
		ImageFormat:                   "oci-tar",
		SandboxImageArtifactID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		SandboxImageArtifactDigest:    "sha256:sandbox",
		SandboxImageArtifactFormat:    "oci-tar",
		WorkspaceMountPath:            "/workspace",
		RuntimeABI:                    arg.RuntimeABI,
		GuestdAbi:                     "guestd-abi",
		AdapterAbi:                    "adapter-abi",
		ReservedCpuMillis:             1000,
		ReservedMemoryMib:             1024,
		ReservedExecutionSlots:        1,
		State:                         db.RuntimeInstanceStatePreparing,
		InstanceToken:                 arg.InstanceToken,
		ExpiresAt:                     arg.ExpiresAt,
		SandboxImageArtifactMediaType: api.SandboxImageArtifactMediaType,
		SandboxImageArtifactSizeBytes: 123,
	}, nil
}

func (f *fakeRuntimePreparerStore) MarkRuntimeInstanceFailed(_ context.Context, _ db.MarkRuntimeInstanceFailedParams) (db.RuntimeInstance, error) {
	return db.RuntimeInstance{}, nil
}

func (f *fakeRuntimePreparerStore) CreateWorkerCommand(_ context.Context, arg db.CreateWorkerCommandParams) (db.WorkerCommand, error) {
	f.commands = append(f.commands, arg)
	return db.WorkerCommand{
		ID:               int64(len(f.commands)),
		OrgID:            arg.OrgID,
		ProjectID:        arg.ProjectID,
		EnvironmentID:    arg.EnvironmentID,
		RunID:            arg.RunID,
		RunWaitID:        arg.RunWaitID,
		RunLeaseID:       arg.RunLeaseID,
		WorkerInstanceID: arg.WorkerInstanceID,
		RunStateVersion:  arg.RunStateVersion,
		Kind:             arg.Kind,
		Payload:          arg.Payload,
	}, nil
}
