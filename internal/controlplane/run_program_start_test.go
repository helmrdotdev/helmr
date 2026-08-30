package controlplane

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestEncodeProgramStartPreservesTaskPayloadPresence(t *testing.T) {
	run, attempt, definition := validTaskProgramStart(t, deployment.SchemaKindNone)
	first, err := encodeProgramStart(run, attempt, nil, definition, "v42")
	if err != nil {
		t.Fatalf("encodeProgramStart: %v", err)
	}
	second, err := encodeProgramStart(run, attempt, nil, definition, "v42")
	if err != nil {
		t.Fatalf("encodeProgramStart replay: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("Program-start encoding is not deterministic")
	}
	var absent programv0.ProgramStart
	if err := frameio.ReadProtoFrame(bytes.NewReader(first), &absent); err != nil {
		t.Fatalf("read Program-start frame: %v", err)
	}
	if absent.GetTask().GetNoPayload() == nil {
		t.Fatal("payload-free Task was not encoded as no_payload")
	}

	run, attempt, definition = validTaskProgramStart(t, deployment.SchemaKindStandard)
	run.Payload = []byte("null")
	body, err := encodeProgramStart(run, attempt, nil, definition, "v42")
	if err != nil {
		t.Fatalf("encodeProgramStart JSON null: %v", err)
	}
	var present programv0.ProgramStart
	if err := frameio.ReadProtoFrame(bytes.NewReader(body), &present); err != nil {
		t.Fatalf("read Program-start JSON null frame: %v", err)
	}
	if got := string(present.GetTask().GetPayloadJson()); got != "null" {
		t.Fatalf("payload_json = %q, want JSON null", got)
	}

	run.Payload = nil
	if _, err := encodeProgramStart(run, attempt, nil, definition, "v42"); err == nil {
		t.Fatal("payload Task accepted absent payload")
	}
}

func TestEncodeProgramStartActorAndScheduleCause(t *testing.T) {
	run, attempt, definition := validTaskProgramStart(t, deployment.SchemaKindNone)
	actorID := pgvalue.UUID(uuid.New())
	run.EntrypointKind = "actor"
	run.EntrypointDeclaredID = "reviewer"
	run.SessionID = actorID
	run.CauseKind = "continuation"
	run.SessionInputStartSequence = pgtype.Int8{Int64: 4, Valid: true}
	run.SessionInputHighWatermark = pgtype.Int8{Int64: 7, Valid: true}
	attempt.EntrypointKind = "actor"
	attempt.SessionInputStartSequence = pgtype.Int8{Int64: 5, Valid: true}
	definition.Kind = "actor"
	definition.DeclaredID = "reviewer"
	key := "repository-17"
	actor := db.Session{
		ID:                     actorID,
		ActorDeclaredID:        "reviewer",
		DeploymentDefinitionID: run.DeploymentDefinitionID,
		WorkspaceID:            run.WorkspaceID,
		Key:                    pgtype.Text{String: key, Valid: true},
	}
	body, err := encodeProgramStart(run, attempt, &actor, definition, "v42")
	if err != nil {
		t.Fatalf("encodeProgramStart Actor: %v", err)
	}
	var message programv0.ProgramStart
	if err := frameio.ReadProtoFrame(bytes.NewReader(body), &message); err != nil {
		t.Fatalf("read Actor Program-start frame: %v", err)
	}
	if message.GetActor().GetSessionId() != pgvalue.UUIDString(actorID) ||
		message.GetActor().GetKey() != key ||
		message.GetActor().GetStartInputSequence() != 5 ||
		message.GetActor().GetInputHighWatermark() != 7 ||
		message.GetCause().GetContinuation() == nil {
		t.Fatalf("unexpected Actor Program-start: %v", &message)
	}

	run, attempt, definition = validTaskProgramStart(t, deployment.SchemaKindNone)
	run.CauseKind = "schedule"
	run.ScheduleID = pgvalue.UUID(uuid.New())
	run.ScheduleGeneration = pgtype.Int8{Int64: 3, Valid: true}
	run.ScheduledAt = pgtype.Timestamptz{Time: time.UnixMilli(1700000000000), Valid: true}
	run.PreviousScheduledAt = pgtype.Timestamptz{Time: time.UnixMilli(1699996400000), Valid: true}
	run.ScheduleTimezone = pgtype.Text{String: "Asia/Tokyo", Valid: true}
	body, err = encodeProgramStart(run, attempt, nil, definition, "v42")
	if err != nil {
		t.Fatalf("encodeProgramStart schedule: %v", err)
	}
	if err := frameio.ReadProtoFrame(bytes.NewReader(body), &message); err != nil {
		t.Fatalf("read schedule Program-start frame: %v", err)
	}
	schedule := message.GetCause().GetSchedule()
	if schedule == nil ||
		schedule.GetScheduledAtUnixMs() != 1700000000000 ||
		schedule.GetPreviousScheduledAtUnixMs() != 1699996400000 ||
		schedule.GetTimezone() != "Asia/Tokyo" {
		t.Fatalf("unexpected schedule cause: %v", schedule)
	}
}

func validTaskProgramStart(
	t *testing.T,
	payloadKind deployment.SchemaKind,
) (db.Run, db.RunAttempt, db.DeploymentDefinition) {
	t.Helper()
	runID := pgvalue.UUID(uuid.New())
	environmentID := pgvalue.UUID(uuid.New())
	deploymentID := pgvalue.UUID(uuid.New())
	definitionID := pgvalue.UUID(uuid.New())
	workspaceID := pgvalue.UUID(uuid.New())
	versionID := pgvalue.UUID(uuid.New())
	raw := []byte(`{"payload":{"kind":"` + string(payloadKind) + `"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`)
	_, digest, err := deployment.CanonicalManifestAndDigest(raw)
	if err != nil {
		t.Fatalf("CanonicalManifestAndDigest: %v", err)
	}
	run := db.Run{
		ID:                     runID,
		EnvironmentID:          environmentID,
		DeploymentID:           deploymentID,
		DeploymentDefinitionID: definitionID,
		EntrypointKind:         "task",
		EntrypointDeclaredID:   "compile",
		CauseKind:              "api",
		WorkspaceID:            workspaceID,
		BaseWorkspaceVersionID: versionID,
		CurrentAttemptNumber:   1,
	}
	attempt := db.RunAttempt{
		RunID:                  runID,
		Number:                 1,
		EntrypointKind:         "task",
		WorkspaceID:            workspaceID,
		BaseWorkspaceVersionID: versionID,
	}
	definition := db.DeploymentDefinition{
		ID:              definitionID,
		EnvironmentID:   environmentID,
		DeploymentID:    deploymentID,
		Kind:            "task",
		DeclaredID:      "compile",
		ManifestVersion: deployment.DeploymentPlanFormatVersion,
		Manifest:        raw,
		ManifestDigest:  digest[:],
	}
	return run, attempt, definition
}
