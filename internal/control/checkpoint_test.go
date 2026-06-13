package control

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCheckpointArtifactParamsValidation(t *testing.T) {
	stateDigest := "sha256:" + strings.Repeat("1", 64)
	memoryDigest := "sha256:" + strings.Repeat("2", 64)
	scratchDigest := "sha256:" + strings.Repeat("3", 64)
	manifestDigest := "sha256:" + strings.Repeat("4", 64)
	valid := api.WorkerCheckpointManifest{
		RuntimeState: api.WorkerCheckpointRuntimeState{
			ConfigArtifact:      testCheckpointArtifact(manifestDigest, 64, cas.CheckpointRuntimeConfigMediaType),
			VMStateArtifact:     testCheckpointArtifact(stateDigest, 128, cas.CheckpointVMStateMediaType),
			ScratchDiskArtifact: testCheckpointArtifact(scratchDigest, 512, cas.CheckpointScratchDiskMediaType),
			MemoryArtifacts:     []api.WorkerCheckpointArtifact{testCheckpointArtifact(memoryDigest, 256, cas.CheckpointMemoryMediaType)},
		},
	}
	if _, err := checkpointArtifactParams(valid); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		manifest api.WorkerCheckpointManifest
		want     string
	}{
		{
			name: "missing state metadata",
			manifest: withCheckpointManifest(valid, func(m *api.WorkerCheckpointManifest) {
				m.RuntimeState.VMStateArtifact = api.WorkerCheckpointArtifact{}
			}),
			want: "manifest.runtime_state.vm_state_artifact.digest",
		},
		{
			name: "wrong memory media type",
			manifest: withCheckpointManifest(valid, func(m *api.WorkerCheckpointManifest) {
				m.RuntimeState.MemoryArtifacts[0].MediaType = cas.CheckpointVMStateMediaType
			}),
			want: "expected",
		},
		{
			name: "wrong manifest media type",
			manifest: withCheckpointManifest(valid, func(m *api.WorkerCheckpointManifest) {
				m.RuntimeState.ConfigArtifact.MediaType = cas.CheckpointMemoryMediaType
			}),
			want: "expected",
		},
		{
			name: "conflicting duplicate metadata",
			manifest: withCheckpointManifest(valid, func(m *api.WorkerCheckpointManifest) {
				m.RuntimeState.MemoryArtifacts = append(m.RuntimeState.MemoryArtifacts, testCheckpointArtifact(memoryDigest, 257, cas.CheckpointMemoryMediaType))
			}),
			want: "conflicting metadata",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := checkpointArtifactParams(tt.manifest)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCheckpointRuntimeBackendFenceMatchesSQL(t *testing.T) {
	source, err := os.ReadFile("../db/query/waitpoints.sql")
	if err != nil {
		t.Fatal(err)
	}
	expectedFence := "sqlc.arg(runtime_backend)::text = '" + checkpointRuntimeBackendFirecracker + "'"
	if !strings.Contains(string(source), expectedFence) {
		t.Fatalf("checkpoint runtime backend SQL fence missing %q", expectedFence)
	}
}

func TestVerifyCheckpointReadyArtifactsRejectsCASMetadataMismatch(t *testing.T) {
	manifest := testWorkerCheckpointManifest("run-1", "waitpoint-1", "checkpoint-1")
	objects := checkpointManifestCASObjects(manifest)
	memory := manifest.RuntimeState.MemoryArtifacts[0]
	objects[memory.Digest] = cas.Object{Digest: memory.Digest, SizeBytes: memory.SizeBytes + 1, MediaType: memory.MediaType}
	server := &Server{cas: &fakeCAS{objects: objects}}

	err := server.verifyCheckpointReadyArtifacts(context.Background(), manifest)

	if err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("err = %v, want size mismatch", err)
	}
}

func testCheckpointArtifact(digest string, sizeBytes int64, mediaType string) api.WorkerCheckpointArtifact {
	return api.WorkerCheckpointArtifact{
		Digest:    digest,
		SizeBytes: sizeBytes,
		MediaType: mediaType,
	}
}

func testWorkerCheckpointManifest(runID string, waitpointID string, checkpointID string) api.WorkerCheckpointManifest {
	runtimeConfig := json.RawMessage(`{"recovery_point":{"runtime":{"vcpu_count":1,"memory_mib":1024,"scratch_disk_mib":2048,"network":{"profile":"helmr/v0"}}}}`)
	capabilities := testWorkerCapabilities()
	return api.WorkerCheckpointManifest{
		RecoveryPoint: api.WorkerCheckpointRecoveryPoint{
			ID:          checkpointID,
			RunID:       runID,
			WaitpointID: waitpointID,
			Runtime: api.WorkerCheckpointRuntime{
				Backend:         "firecracker",
				ID:              capabilities.RuntimeID,
				Arch:            capabilities.RuntimeArch,
				ABI:             capabilities.RuntimeABI,
				KernelDigest:    capabilities.KernelDigest,
				InitramfsDigest: capabilities.InitramfsDigest,
				RootfsDigest:    capabilities.RootfsDigest,
				ConfigDigest:    sha256sum.DigestBytes(runtimeConfig),
			},
		},
		RuntimeState: api.WorkerCheckpointRuntimeState{
			ConfigArtifact:      testCheckpointArtifact("sha256:"+strings.Repeat("7", 64), int64(len(runtimeConfig)), cas.CheckpointRuntimeConfigMediaType),
			VMStateArtifact:     testCheckpointArtifact("sha256:"+strings.Repeat("1", 64), 128, cas.CheckpointVMStateMediaType),
			ScratchDiskArtifact: testCheckpointArtifact("sha256:"+strings.Repeat("6", 64), 512, cas.CheckpointScratchDiskMediaType),
			MemoryArtifacts:     []api.WorkerCheckpointArtifact{testCheckpointArtifact("sha256:"+strings.Repeat("2", 64), 256, cas.CheckpointMemoryMediaType)},
			Config:              runtimeConfig,
		},
		WorkspaceState: api.WorkerCheckpointWorkspaceState{Base: api.WorkerCheckpointWorkspaceBase{
			ArtifactDigest:    "sha256:" + strings.Repeat("8", 64),
			ArtifactSizeBytes: 1024,
			ArtifactMediaType: "application/vnd.helmr.workspace.v0.tar",
			ArtifactEncoding:  "tar",
			MountPath:         "/workspace",
			VolumeKind:        "copy-on-write",
		}},
	}
}

func checkpointManifestCASObjects(manifest api.WorkerCheckpointManifest) map[string]cas.Object {
	objects := map[string]cas.Object{}
	add := func(digest string, sizeBytes int64, mediaType string) {
		objects[digest] = cas.Object{Digest: digest, SizeBytes: sizeBytes, MediaType: mediaType}
	}
	add(manifest.RuntimeState.ConfigArtifact.Digest, manifest.RuntimeState.ConfigArtifact.SizeBytes, manifest.RuntimeState.ConfigArtifact.MediaType)
	add(manifest.RuntimeState.VMStateArtifact.Digest, manifest.RuntimeState.VMStateArtifact.SizeBytes, manifest.RuntimeState.VMStateArtifact.MediaType)
	add(manifest.RuntimeState.ScratchDiskArtifact.Digest, manifest.RuntimeState.ScratchDiskArtifact.SizeBytes, manifest.RuntimeState.ScratchDiskArtifact.MediaType)
	for _, artifact := range manifest.RuntimeState.MemoryArtifacts {
		add(artifact.Digest, artifact.SizeBytes, artifact.MediaType)
	}
	workspace := manifest.WorkspaceState.Base
	add(workspace.ArtifactDigest, workspace.ArtifactSizeBytes, workspace.ArtifactMediaType)
	return objects
}

func withCheckpointManifest(manifest api.WorkerCheckpointManifest, edit func(*api.WorkerCheckpointManifest)) api.WorkerCheckpointManifest {
	manifest.RuntimeState.MemoryArtifacts = append([]api.WorkerCheckpointArtifact(nil), manifest.RuntimeState.MemoryArtifacts...)
	edit(&manifest)
	return manifest
}

func (f *fakeStore) UpsertCasObject(_ context.Context, arg db.UpsertCasObjectParams) (db.CasObject, error) {
	f.casObjects = append(f.casObjects, arg)
	return db.CasObject{
		Digest:    arg.Digest,
		SizeBytes: arg.SizeBytes,
		MediaType: arg.MediaType,
		CreatedAt: testTime(),
	}, nil
}

func (f *fakeStore) GetCasObject(_ context.Context, digest string) (db.CasObject, error) {
	if f.getCasObjectErr != nil {
		return db.CasObject{}, f.getCasObjectErr
	}
	for _, object := range f.casObjects {
		if object.Digest == digest {
			return db.CasObject{
				Digest:    object.Digest,
				SizeBytes: object.SizeBytes,
				MediaType: object.MediaType,
				CreatedAt: testTime(),
			}, nil
		}
	}
	return db.CasObject{}, pgx.ErrNoRows
}

func (f *fakeStore) MarkWaitpointCheckpointDurableReady(_ context.Context, arg db.MarkWaitpointCheckpointDurableReadyParams) (db.MarkWaitpointCheckpointDurableReadyRow, error) {
	if f.sessionID != arg.SessionID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.MarkWaitpointCheckpointDurableReadyRow{}, pgx.ErrNoRows
	}
	if !f.waitpoint.ID.Valid || f.waitpoint.ID != arg.WaitpointID || waitpointRunWaitID(f.waitpoint) != arg.RunWaitID || f.waitpoint.CheckpointID != arg.CheckpointID || f.waitpoint.Status != db.RunWaitStatusOpening {
		return db.MarkWaitpointCheckpointDurableReadyRow{}, pgx.ErrNoRows
	}
	f.waitpoint.Status = db.RunWaitStatusWaiting
	f.waitpoint.RequestedAt = testTime()
	f.checkpoint = db.Checkpoint{
		ID:        arg.CheckpointID,
		OrgID:     arg.OrgID,
		RunID:     arg.RunID,
		SessionID: arg.SessionID,
		Status:    db.CheckpointStatusReady,
		Manifest:  arg.Manifest,
		ReadyAt:   testTime(),
	}
	f.run.Status = db.RunStatusWaiting
	f.run.LatestCheckpointID = arg.CheckpointID
	f.run.CurrentSessionID = pgtype.UUID{}
	f.run.UpdatedAt = testTime()
	f.events = append(f.events, db.Event{
		Seq:       int64(len(f.events) + 1),
		OrgID:     arg.OrgID,
		RunID:     arg.RunID,
		Kind:      "checkpoint.ready",
		Payload:   arg.CheckpointPayload,
		CreatedAt: testTime(),
	})
	f.events = append(f.events, db.Event{
		Seq:       int64(len(f.events) + 1),
		OrgID:     arg.OrgID,
		RunID:     arg.RunID,
		Kind:      "waitpoint.requested",
		Payload:   []byte(`{"kind":"human"}`),
		CreatedAt: testTime(),
	})
	return db.MarkWaitpointCheckpointDurableReadyRow{
		ID:             f.waitpoint.ID,
		RunWaitID:      waitpointRunWaitID(f.waitpoint),
		OrgID:          f.waitpoint.OrgID,
		RunID:          f.waitpoint.RunID,
		SessionID:      f.waitpoint.SessionID,
		CheckpointID:   f.waitpoint.CheckpointID,
		CorrelationID:  f.waitpoint.CorrelationID,
		Kind:           f.waitpoint.Kind,
		Request:        f.waitpoint.Request,
		DisplayText:    f.waitpoint.DisplayText,
		TimeoutSeconds: f.waitpoint.TimeoutSeconds,
		PolicyName:     f.waitpoint.PolicyName,
		PolicySnapshot: f.waitpoint.PolicySnapshot,
		Status:         f.waitpoint.Status,
		ResolutionKind: f.waitpoint.ResolutionKind,
		Resolution:     f.waitpoint.Resolution,
		RequestedAt:    f.waitpoint.RequestedAt,
		ResolvedAt:     f.waitpoint.ResolvedAt,
	}, nil
}

func (f *fakeStore) MarkWaitpointCheckpointFailed(_ context.Context, arg db.MarkWaitpointCheckpointFailedParams) (db.MarkWaitpointCheckpointFailedRow, error) {
	if f.sessionID != arg.SessionID || f.executionWorkerInstanceID != arg.WorkerInstanceID || !f.waitpoint.ID.Valid || f.waitpoint.ID != arg.WaitpointID || waitpointRunWaitID(f.waitpoint) != arg.RunWaitID || f.waitpoint.CheckpointID != arg.CheckpointID || f.waitpoint.Status != db.RunWaitStatusOpening {
		return db.MarkWaitpointCheckpointFailedRow{}, pgx.ErrNoRows
	}
	f.waitpoint.Status = db.RunWaitStatusCancelled
	f.waitpoint.ResolutionKind = pgtype.Text{String: "cancelled", Valid: true}
	f.waitpoint.Resolution = []byte(`{"source":"checkpoint"}`)
	f.waitpoint.ResolvedAt = testTime()
	return db.MarkWaitpointCheckpointFailedRow{
		ID:             f.waitpoint.ID,
		RunWaitID:      waitpointRunWaitID(f.waitpoint),
		OrgID:          f.waitpoint.OrgID,
		RunID:          f.waitpoint.RunID,
		SessionID:      f.waitpoint.SessionID,
		CheckpointID:   f.waitpoint.CheckpointID,
		CorrelationID:  f.waitpoint.CorrelationID,
		Kind:           f.waitpoint.Kind,
		Request:        f.waitpoint.Request,
		DisplayText:    f.waitpoint.DisplayText,
		TimeoutSeconds: f.waitpoint.TimeoutSeconds,
		PolicyName:     f.waitpoint.PolicyName,
		PolicySnapshot: f.waitpoint.PolicySnapshot,
		Status:         f.waitpoint.Status,
		ResolutionKind: f.waitpoint.ResolutionKind,
		Resolution:     f.waitpoint.Resolution,
		RequestedAt:    f.waitpoint.RequestedAt,
		ResolvedAt:     f.waitpoint.ResolvedAt,
	}, nil
}

func (f *fakeStore) GetRunRestorePayload(_ context.Context, arg db.GetRunRestorePayloadParams) (db.GetRunRestorePayloadRow, error) {
	if f.run.OrgID != arg.OrgID || f.run.ID != arg.RunID || f.run.CurrentSessionID != arg.SessionID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.GetRunRestorePayloadRow{}, pgx.ErrNoRows
	}
	if f.run.LatestCheckpointID != f.checkpoint.ID || f.checkpoint.Status != db.CheckpointStatusRestoring {
		return db.GetRunRestorePayloadRow{}, pgx.ErrNoRows
	}
	if !f.waitpoint.ID.Valid || f.waitpoint.Status != db.RunWaitStatusResuming || !f.waitpoint.ResolutionKind.Valid || f.waitpoint.CheckpointID != f.checkpoint.ID {
		return db.GetRunRestorePayloadRow{}, pgx.ErrNoRows
	}
	return db.GetRunRestorePayloadRow{
		CheckpointID:   f.checkpoint.ID,
		Manifest:       f.checkpoint.Manifest,
		RunWaitID:      waitpointRunWaitID(f.waitpoint),
		WaitpointID:    f.waitpoint.ID,
		WaitpointKind:  f.waitpoint.Kind,
		ResolutionKind: f.waitpoint.ResolutionKind,
		Resolution:     f.waitpoint.Resolution,
	}, nil
}
