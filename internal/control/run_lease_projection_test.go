package control

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestProjectSecretDeliveriesUsesCanonicalPlacementOrder(t *testing.T) {
	deliveries, err := projectSecretDeliveries([]secret.DeliveryMaterial{
		{PlacementKind: "file", PlacementTarget: "/run/token", Value: []byte("file-value")},
		{PlacementKind: "env", PlacementTarget: "TOKEN", Value: []byte("env-value")},
	})
	if err != nil {
		t.Fatalf("projectSecretDeliveries: %v", err)
	}
	if len(deliveries) != 2 ||
		deliveries[0].Env == nil ||
		deliveries[0].Env.Name != "TOKEN" ||
		deliveries[1].File == nil ||
		deliveries[1].File.Path != "/run/token" {
		t.Fatalf("unexpected Secret delivery order: %#v", deliveries)
	}
}

func TestProjectRunLeaseExecutionKeepsFreshAndParentAttachClosed(t *testing.T) {
	run, attempt, definition := validTaskProgramStart(t, "none")
	fresh, err := projectRunLeaseExecution(runLeaseExecutionProjection{
		mode: runLeaseClaimFresh, run: run, attempt: attempt,
		definition: definition, deploymentVersion: "v42",
	})
	if err != nil {
		t.Fatalf("project fresh execution: %v", err)
	}
	if fresh.Fresh == nil || fresh.Restore != nil || fresh.Attach != nil {
		t.Fatalf("unexpected fresh execution union: %#v", fresh)
	}

	waitID := pgvalue.UUID(uuid.New())
	checkpointID := pgvalue.UUID(uuid.New())
	attachID := pgvalue.UUID(uuid.New())
	attempt.EntrypointEnteredAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	parent, err := projectRunLeaseExecution(runLeaseExecutionProjection{
		mode:    runLeaseClaimAttachParent,
		run:     run,
		attempt: attempt,
		runWait: db.RunWait{
			ID: waitID, ConditionState: db.WaitStateCompleted,
			ConditionTerminalAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			ResumeAttachID:      attachID, ResumeRequestVersion: 2,
		},
		checkpoint: db.RunCheckpoint{ID: checkpointID},
		childRun:   db.Run{ID: pgvalue.UUID(uuid.New())},
	})
	if err != nil {
		t.Fatalf("project parent attach execution: %v", err)
	}
	if parent.Fresh != nil ||
		parent.Restore != nil ||
		parent.Attach == nil ||
		parent.Attach.Child != nil ||
		parent.Attach.Parent == nil ||
		parent.Attach.Parent.ResumeAttachID != pgvalue.UUIDString(attachID) {
		t.Fatalf("unexpected parent attach execution union: %#v", parent)
	}
}

func TestProjectRunWaitDecisionDistinguishesAbsentAndJSONNull(t *testing.T) {
	terminalAt := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	absent, err := projectRunWaitDecision(db.RunWait{
		ConditionState: db.WaitStateCompleted, ConditionTerminalAt: terminalAt,
	})
	if err != nil {
		t.Fatalf("project absent result: %v", err)
	}
	if absent.Completed == nil ||
		absent.Completed.NoResult == nil ||
		absent.Completed.ResultJSON != nil {
		t.Fatalf("unexpected absent result projection: %#v", absent)
	}

	present, err := projectRunWaitDecision(db.RunWait{
		ConditionState:      db.WaitStateCompleted,
		ConditionResult:     []byte("null"),
		ConditionTerminalAt: terminalAt,
	})
	if err != nil {
		t.Fatalf("project JSON null: %v", err)
	}
	if present.Completed == nil ||
		present.Completed.NoResult != nil ||
		present.Completed.ResultJSON == nil ||
		string(present.Completed.ResultJSON) != "null" {
		t.Fatalf("unexpected JSON null projection: %#v", present)
	}

	payload, err := json.Marshal(present)
	if err != nil {
		t.Fatalf("marshal decision: %v", err)
	}
	if string(payload) != `{"completed":{"result_json":null}}` {
		t.Fatalf("decision JSON = %s", payload)
	}
	var roundTrip api.WorkerRunLeaseDecision
	if err := json.Unmarshal(payload, &roundTrip); err != nil {
		t.Fatalf("unmarshal decision: %v", err)
	}
	if roundTrip.Completed == nil ||
		roundTrip.Completed.NoResult != nil ||
		string(roundTrip.Completed.ResultJSON) != "null" {
		t.Fatalf("JSON null presence was lost: %#v", roundTrip)
	}
}

func TestProjectRunLeaseCheckpointRequiresCanonicalArtifactAuthority(t *testing.T) {
	checkpoint := db.RunCheckpoint{
		ID:              pgvalue.UUID(uuid.New()),
		Kind:            db.RunCheckpointKindSuspend,
		State:           db.RunCheckpointStateReady,
		RestoreManifest: []byte(`{"version":0}`),
	}
	rows := []db.ListRunCheckpointArtifactAuthorityRow{
		{
			Role: db.RunCheckpointArtifactRoleRuntimeConfig, Ordinal: 0,
			Digest: validDigest('a'), SizeBytes: 8, MediaType: "application/example",
		},
		{
			Role: db.RunCheckpointArtifactRoleVmState, Ordinal: 0,
			Digest: validDigest('b'), SizeBytes: 4, MediaType: "application/example",
		},
		{
			Role: db.RunCheckpointArtifactRoleMemory, Ordinal: 0,
			Digest: validDigest('c'), SizeBytes: 16, MediaType: "application/example",
		},
	}
	projected, err := projectRunLeaseCheckpoint(checkpoint, rows)
	if err != nil {
		t.Fatalf("projectRunLeaseCheckpoint: %v", err)
	}
	if len(projected.Artifacts) != 3 ||
		projected.Artifacts[0].Role != "runtime_config" ||
		projected.Artifacts[1].Role != "vm_state" ||
		projected.Artifacts[2].Role != "memory" {
		t.Fatalf("unexpected checkpoint Artifacts: %#v", projected.Artifacts)
	}

	rows[2].Role = db.RunCheckpointArtifactRoleRuntimeConfig
	if _, err := projectRunLeaseCheckpoint(checkpoint, rows); err == nil {
		t.Fatal("noncanonical checkpoint Artifact authority was accepted")
	}
}

func TestProjectRunLeaseReceiptAndWorkspace(t *testing.T) {
	authority := validRunLeaseProjectionAuthority()
	receipt, err := projectRunLeaseReceipt(authority)
	if err != nil {
		t.Fatalf("projectRunLeaseReceipt: %v", err)
	}
	if receipt.LeaseSequence != 2 ||
		receipt.WorkspaceID != pgvalue.UUIDString(authority.workspace.ID) ||
		receipt.WorkspaceLeaseID != pgvalue.UUIDString(authority.workspaceLease.ID) ||
		receipt.BaseWorkspaceVersionID != pgvalue.UUIDString(authority.workspaceLease.BaseVersionID) ||
		receipt.OwnershipGeneration != authority.workspaceLease.OwnershipGeneration ||
		receipt.WriterGeneration != authority.workspaceLease.WriterGeneration ||
		receipt.MountFencingGeneration != authority.workspaceLease.MountFencingGeneration ||
		receipt.MaxActiveDurationMs != authority.run.MaxActiveDurationMs {
		t.Fatalf("unexpected Run Lease receipt: %#v", receipt)
	}
	authority.runLease.StartDeadlineAt = authority.runLease.ExpiresAt
	if _, err := projectRunLeaseReceipt(authority); err != nil {
		t.Fatalf("equal Run Lease deadlines: %v", err)
	}
	resetAuthority := validWorkspaceResetTargetAuthority(authority)
	workspace, err := projectWorkspaceAttachment(authority, "write-capability", resetAuthority)
	if err != nil {
		t.Fatalf("projectWorkspaceAttachment: %v", err)
	}
	if workspace.WriteCapability != "write-capability" || workspace.ResetTarget.Empty == nil ||
		workspace.ResetTarget.BaseWorkspaceVersionID != receipt.BaseWorkspaceVersionID {
		t.Fatalf("unexpected Workspace attachment: %#v", workspace)
	}

	authority.workspaceLease.WriterGeneration++
	if _, err := projectWorkspaceAttachment(authority, "write-capability", resetAuthority); err == nil {
		t.Fatal("mismatched writer generation was accepted")
	}
	authority.workspaceLease.RuntimeInstanceID = pgvalue.UUID(uuid.New())
	if _, err := projectWorkspaceAttachment(authority, "write-capability", resetAuthority); err == nil {
		t.Fatal("mismatched Workspace Lease runtime was accepted")
	}
}

func validWorkspaceResetTargetAuthority(
	authority runLeaseProjectionAuthority,
) db.GetWorkspaceResetTargetAuthorityRow {
	return db.GetWorkspaceResetTargetAuthorityRow{
		VersionID:     authority.workspaceLease.BaseVersionID,
		VersionKind:   db.WorkspaceVersionKindSystem,
		ContentDigest: workspace.CanonicalEmptyTreeDigest,
	}
}

func TestProjectWorkspaceAttachmentProjectsArtifactResetTarget(t *testing.T) {
	authority := validRunLeaseProjectionAuthority()
	resetAuthority := db.GetWorkspaceResetTargetAuthorityRow{
		VersionID:       authority.workspaceLease.BaseVersionID,
		ParentVersionID: pgvalue.UUID(uuid.New()), ArtifactID: pgvalue.UUID(uuid.New()),
		ArtifactKind: db.NullArtifactKind{ArtifactKind: db.ArtifactKindWorkspaceVersion, Valid: true},
		VersionKind:  db.WorkspaceVersionKindUser, ContentDigest: validDigest('c'),
		LogicalSizeBytes: 3, EntryCount: 1,
		SourceWorkspaceLeaseID: pgvalue.UUID(uuid.New()), OwnershipGeneration: 5, WriterGeneration: 6,
		ArtifactRowKind:   db.NullArtifactKind{ArtifactKind: db.ArtifactKindWorkspaceVersion, Valid: true},
		ArtifactDigest:    pgvalue.Text(validDigest('d')),
		ArtifactSizeBytes: pgtype.Int8{Int64: 1024, Valid: true},
		ArtifactMediaType: pgvalue.Text(workspace.ArtifactMediaType),
	}
	attachment, err := projectWorkspaceAttachment(authority, "write-capability", resetAuthority)
	if err != nil {
		t.Fatal(err)
	}
	if attachment.ResetTarget.Artifact == nil || attachment.ResetTarget.Empty != nil ||
		attachment.ResetTarget.Artifact.Digest != validDigest('d') ||
		attachment.ResetTarget.Tree.Digest != validDigest('c') {
		t.Fatalf("attachment = %#v", attachment)
	}

	resetAuthority.ArtifactRowKind = db.NullArtifactKind{}
	if _, err := projectWorkspaceAttachment(authority, "write-capability", resetAuthority); err == nil {
		t.Fatal("partial Workspace Artifact relation was accepted")
	}
}

func validRunLeaseProjectionAuthority() runLeaseProjectionAuthority {
	runID := pgvalue.UUID(uuid.New())
	workspaceID := pgvalue.UUID(uuid.New())
	versionID := pgvalue.UUID(uuid.New())
	attemptNumber := int32(1)
	runtimeID := pgvalue.UUID(uuid.New())
	slotID := pgvalue.UUID(uuid.New())
	mountID := pgvalue.UUID(uuid.New())
	runLeaseID := pgvalue.UUID(uuid.New())
	workspaceLeaseID := pgvalue.UUID(uuid.New())
	now := time.Now().UTC()
	return runLeaseProjectionAuthority{
		run: db.Run{
			ID: runID, WorkspaceID: workspaceID, CurrentAttemptNumber: attemptNumber,
			MaxActiveDurationMs: 300000, ActiveElapsedMs: 1000,
		},
		attempt:     db.RunAttempt{RunID: runID, Number: attemptNumber},
		runtime:     db.RuntimeInstance{ID: runtimeID},
		networkSlot: db.WorkerNetworkSlot{ID: slotID},
		runLease: db.RunLease{
			ID: runLeaseID, RunID: runID, WorkspaceID: workspaceID,
			AttemptNumber: attemptNumber, LeaseSequence: 2,
			WorkerGroupID: "workers", WorkerInstanceID: pgvalue.UUID(uuid.New()),
			WorkerEpoch: 3, WorkerProtocolVersion: "helmr.worker.v0",
			RuntimeInstanceID: runtimeID, RuntimeIdentityID: "runtime",
			NetworkSlotID: slotID, NetworkSlotGeneration: 4,
			RequestedCpuMillis: 1000, RequestedMemoryBytes: 1024,
			RequestedWorkloadDiskBytes: 2048, RequestedScratchBytes: 4096,
			RequestedExecutionSlots: 1,
			StartDeadlineAt:         pgtype.Timestamptz{Time: now.Add(time.Minute), Valid: true},
			ExpiresAt:               pgtype.Timestamptz{Time: now.Add(5 * time.Minute), Valid: true},
		},
		workspace: db.Workspace{
			ID: workspaceID, OwnershipGeneration: 5, WriterGeneration: 6,
		},
		workspaceMount: db.WorkspaceMount{
			ID: mountID, WorkspaceID: workspaceID,
			RuntimeInstanceID: runtimeID, MaterializedVersionID: versionID,
			FencingGeneration: 7,
		},
		workspaceLease: db.WorkspaceLease{
			ID: workspaceLeaseID, OwnerRunLeaseID: runLeaseID,
			WorkspaceID: workspaceID, RuntimeInstanceID: runtimeID,
			WorkspaceMountID: mountID, BaseVersionID: versionID,
			OwnershipGeneration: 5, WriterGeneration: 6, MountFencingGeneration: 7,
		},
	}
}

func validDigest(fill byte) string {
	value := make([]byte, 64)
	for index := range value {
		value[index] = fill
	}
	return "sha256:" + string(value)
}
