package controlplane

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func captureProjectionFixture(count int) (db.ComputerCheckpoint, []db.ComputerCheckpointRun) {
	cp := db.ComputerCheckpoint{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(uuid.NewV7()), ComputerID: pgvalue.UUID(uuid.NewV7()), ComputerSpecID: pgvalue.UUID(uuid.NewV7()), SourceComputerInstanceID: pgvalue.UUID(uuid.NewV7()), WriterGeneration: 3, MembershipRevision: 7, Status: "creating"}
	if count > 0 {
		cp.ProgramDeploymentID = pgvalue.UUID(uuid.NewV7())
	}
	members := make([]db.ComputerCheckpointRun, 0, count)
	for range count {
		members = append(members, db.ComputerCheckpointRun{CheckpointID: cp.ID, EnvironmentID: cp.EnvironmentID, ComputerID: cp.ComputerID, SourceComputerInstanceID: cp.SourceComputerInstanceID, WriterGeneration: cp.WriterGeneration, RunID: pgvalue.UUID(uuid.NewV7()), AttemptNumber: 2, RunWaitID: pgvalue.UUID(uuid.NewV7()), SourceRunLeaseID: pgvalue.UUID(uuid.NewV7())})
	}
	return cp, members
}

func TestComputerCaptureProjection(t *testing.T) {
	for _, count := range []int{0, 2} {
		cp, members := captureProjectionFixture(count)
		if count > 0 {
			members[0].ActorSpeculativeInputSequence = pgtype.Int8{Int64: 4, Valid: true}
		}
		got, err := projectComputerInstanceCapture(cp, members)
		if err != nil {
			t.Fatal(err)
		}
		if got.CheckpointID != pgvalue.UUIDString(cp.ID) || got.MembershipRevision != 7 || got.ProgramDeploymentID != pgvalue.UUIDString(cp.ProgramDeploymentID) || got.Runs == nil || len(got.Runs) != count {
			t.Fatalf("capture=%+v", got)
		}
		for n, member := range members {
			run := got.Runs[n]
			if run.RunID != pgvalue.UUIDString(member.RunID) || run.AttemptNumber != member.AttemptNumber || run.RunWaitID != pgvalue.UUIDString(member.RunWaitID) || run.RunLeaseID != pgvalue.UUIDString(member.SourceRunLeaseID) {
				t.Fatalf("member=%+v", run)
			}
		}
		if count > 0 {
			if got.Runs[0].ActorSpeculativeInputSequence == nil || *got.Runs[0].ActorSpeculativeInputSequence != 4 || got.Runs[1].ActorSpeculativeInputSequence != nil {
				t.Fatal("cursor ownership changed")
			}
			*got.Runs[0].ActorSpeculativeInputSequence = 5
			if members[0].ActorSpeculativeInputSequence.Int64 != 4 {
				t.Fatal("projection aliases source")
			}
		}
	}
}

func TestComputerCaptureProjectionRejectsInvalidSet(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*db.ComputerCheckpoint, []db.ComputerCheckpointRun)
	}{
		{"no program", func(c *db.ComputerCheckpoint, m []db.ComputerCheckpointRun) { c.ProgramDeploymentID = pgtype.UUID{} }},
		{"finished checkpoint", func(c *db.ComputerCheckpoint, m []db.ComputerCheckpointRun) { c.Status = "ready" }},
		{"duplicate", func(c *db.ComputerCheckpoint, m []db.ComputerCheckpointRun) { m[1] = m[0] }},
		{"wrong instance", func(c *db.ComputerCheckpoint, m []db.ComputerCheckpointRun) {
			m[0].SourceComputerInstanceID = pgvalue.UUID(uuid.NewV7())
		}},
		{"wrong checkpoint", func(c *db.ComputerCheckpoint, m []db.ComputerCheckpointRun) {
			m[0].CheckpointID = pgvalue.UUID(uuid.NewV7())
		}},
		{"wrong environment", func(c *db.ComputerCheckpoint, m []db.ComputerCheckpointRun) {
			m[0].EnvironmentID = pgvalue.UUID(uuid.NewV7())
		}},
		{"wrong generation", func(c *db.ComputerCheckpoint, m []db.ComputerCheckpointRun) { m[0].WriterGeneration++ }},
		{"missing lease", func(c *db.ComputerCheckpoint, m []db.ComputerCheckpointRun) { m[0].SourceRunLeaseID = pgtype.UUID{} }},
		{"negative cursor", func(c *db.ComputerCheckpoint, m []db.ComputerCheckpointRun) {
			m[0].ActorSpeculativeInputSequence = pgtype.Int8{Int64: -1, Valid: true}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cp, m := captureProjectionFixture(2)
			test.change(&cp, m)
			if _, err := projectComputerInstanceCapture(cp, m); err == nil {
				t.Fatal("invalid set accepted")
			}
		})
	}
}
