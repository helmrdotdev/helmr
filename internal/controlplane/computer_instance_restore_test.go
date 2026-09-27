package controlplane

import (
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func restoreProjectionFixture(t *testing.T, count int) (db.GetComputerInstanceRestoreCheckpointRow, []db.ComputerCheckpointRun, workerapi.CheckpointManifest) {
	t.Helper()
	cp := db.ComputerCheckpoint{ID: pgvalue.UUID(uuid.NewV7()), ComputerID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(uuid.NewV7()), ComputerSpecID: pgvalue.UUID(uuid.NewV7()), SourceComputerInstanceID: pgvalue.UUID(uuid.NewV7()), WriterGeneration: 3, MembershipRevision: 5, Status: "ready"}
	if count > 0 {
		cp.ProgramDeploymentID = pgvalue.UUID(uuid.NewV7())
	}
	point := workerapi.CheckpointRecoveryPoint{ID: pgvalue.UUIDString(cp.ID), ComputerID: pgvalue.UUIDString(cp.ComputerID), ComputerSpecID: pgvalue.UUIDString(cp.ComputerSpecID), ComputerInstanceID: pgvalue.UUIDString(cp.SourceComputerInstanceID), WriterGeneration: 3, MembershipRevision: 5, ProgramDeploymentID: pgvalue.UUIDString(cp.ProgramDeploymentID), Runs: []workerapi.CheckpointRun{}}
	members := make([]db.ComputerCheckpointRun, 0, count)
	for range count {
		member := db.ComputerCheckpointRun{CheckpointID: cp.ID, EnvironmentID: cp.EnvironmentID, ComputerID: cp.ComputerID, SourceComputerInstanceID: cp.SourceComputerInstanceID, WriterGeneration: 3, RunID: pgvalue.UUID(uuid.NewV7()), AttemptNumber: 2, RunWaitID: pgvalue.UUID(uuid.NewV7()), SourceRunLeaseID: pgvalue.UUID(uuid.NewV7())}
		members = append(members, member)
		point.Runs = append(point.Runs, workerapi.CheckpointRun{RunID: pgvalue.UUIDString(member.RunID), AttemptNumber: 2, RunWaitID: pgvalue.UUIDString(member.RunWaitID), RunLeaseID: pgvalue.UUIDString(member.SourceRunLeaseID), CorrelationID: uuid.NewV7().String()})
	}
	descriptor := func(role, media string) workerapi.CheckpointArtifact {
		return workerapi.CheckpointArtifact{Digest: dbtest.Digest(role), SizeBytes: 10, MediaType: media}
	}
	state := workerapi.CheckpointRuntimeState{ConfigArtifact: descriptor("config", cas.CheckpointVMConfigMediaType), VMStateArtifact: descriptor("state", cas.CheckpointVMStateMediaType), MemoryArtifacts: []workerapi.CheckpointArtifact{descriptor("memory", cas.CheckpointMemoryMediaType)}, ScratchDiskArtifact: descriptor("scratch", cas.CheckpointScratchDiskMediaType)}
	authority := db.GetComputerInstanceRestoreCheckpointRow{ComputerCheckpoint: cp, VMConfigDigest: state.ConfigArtifact.Digest, VMConfigSizeBytes: 10, VMConfigMediaType: state.ConfigArtifact.MediaType, VMStateDigest: state.VMStateArtifact.Digest, VMStateSizeBytes: 10, VMStateMediaType: state.VMStateArtifact.MediaType, MemoryDigest: state.MemoryArtifacts[0].Digest, MemorySizeBytes: 10, MemoryMediaType: state.MemoryArtifacts[0].MediaType, ScratchDiskDigest: state.ScratchDiskArtifact.Digest, ScratchDiskSizeBytes: 10, ScratchDiskMediaType: state.ScratchDiskArtifact.MediaType}
	return authority, members, workerapi.CheckpointManifest{RecoveryPoint: point, RuntimeState: state}
}

func encodeRestoreManifest(t *testing.T, a *db.GetComputerInstanceRestoreCheckpointRow, m workerapi.CheckpointManifest) {
	t.Helper()
	var err error
	a.ComputerCheckpoint.Manifest, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
}

func TestComputerRestoreProjection(t *testing.T) {
	for _, count := range []int{0, 2} {
		a, members, manifest := restoreProjectionFixture(t, count)
		encodeRestoreManifest(t, &a, manifest)
		got, err := projectComputerInstanceRestore(a, members)
		if err != nil || got.CheckpointID != manifest.RecoveryPoint.ID || len(got.Artifacts) != 4 {
			t.Fatalf("count=%d restore=%+v error=%v", count, got, err)
		}
		got.Manifest[0] = 'x'
		if a.ComputerCheckpoint.Manifest[0] != '{' {
			t.Fatal("projection aliases persisted manifest")
		}
	}
}

func TestComputerRestoreProjectionRejectsChangedCapture(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*workerapi.CheckpointManifest)
	}{
		{"computer", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.ComputerID = uuid.NewV7().String() }},
		{"instance", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.ComputerInstanceID = uuid.NewV7().String() }},
		{"generation", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.WriterGeneration++ }},
		{"membership", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.MembershipRevision++ }},
		{"program", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.ProgramDeploymentID = uuid.NewV7().String() }},
		{"spec", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.ComputerSpecID = uuid.NewV7().String() }},
		{"missing member", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.Runs = m.RecoveryPoint.Runs[:1] }},
		{"duplicate member", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.Runs[1] = m.RecoveryPoint.Runs[0] }},
		{"attempt", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.Runs[0].AttemptNumber++ }},
		{"wait", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.Runs[0].RunWaitID = uuid.NewV7().String() }},
		{"lease", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.Runs[0].RunLeaseID = uuid.NewV7().String() }},
		{"correlation", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.Runs[0].CorrelationID = "" }},
		{"actor cursor", func(m *workerapi.CheckpointManifest) {
			v := int64(2)
			m.RecoveryPoint.Runs[0].ActorSpeculativeInputSequence = &v
		}},
		{"artifact", func(m *workerapi.CheckpointManifest) {
			m.RuntimeState.MemoryArtifacts[0].Digest = dbtest.Digest("wrong-memory")
		}},
		{"memory count", func(m *workerapi.CheckpointManifest) { m.RuntimeState.MemoryArtifacts = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, members, m := restoreProjectionFixture(t, 2)
			test.change(&m)
			encodeRestoreManifest(t, &a, m)
			if _, err := projectComputerInstanceRestore(a, members); err == nil {
				t.Fatal("changed capture accepted")
			}
		})
	}
}

func TestComputerRestoreProjectionPreservesActorCursor(t *testing.T) {
	a, members, m := restoreProjectionFixture(t, 1)
	members[0].ActorSpeculativeInputSequence = pgtype.Int8{Int64: 7, Valid: true}
	v := int64(7)
	m.RecoveryPoint.Runs[0].ActorSpeculativeInputSequence = &v
	encodeRestoreManifest(t, &a, m)
	if _, err := projectComputerInstanceRestore(a, members); err != nil {
		t.Fatal(err)
	}
	v = 8
	encodeRestoreManifest(t, &a, m)
	if _, err := projectComputerInstanceRestore(a, members); err == nil {
		t.Fatal("changed Actor cursor accepted")
	}
}
