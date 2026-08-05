package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/proto"
)

func encodeProgramStart(
	run db.Run,
	attempt db.RunAttempt,
	actor *db.Session,
	definition db.DeploymentDefinition,
	deploymentVersion string,
) ([]byte, error) {
	runID, err := requiredClaimUUIDString("run ID", run.ID)
	if err != nil {
		return nil, err
	}
	deploymentID, err := requiredClaimUUIDString("deployment ID", run.DeploymentID)
	if err != nil {
		return nil, err
	}
	workspaceID, err := requiredClaimUUIDString("workspace ID", run.WorkspaceID)
	if err != nil {
		return nil, err
	}
	baseVersionID, err := requiredClaimUUIDString("base workspace version ID", attempt.BaseWorkspaceVersionID)
	if err != nil {
		return nil, err
	}
	if attempt.RunID != run.ID ||
		attempt.Number <= 0 ||
		attempt.Number != run.CurrentAttemptNumber ||
		attempt.EntrypointKind != run.EntrypointKind ||
		attempt.WorkspaceID != run.WorkspaceID ||
		definition.ID != run.DeploymentDefinitionID ||
		definition.EnvironmentID != run.EnvironmentID ||
		definition.DeploymentID != run.DeploymentID ||
		definition.Kind != run.EntrypointKind ||
		definition.DeclaredID != run.EntrypointDeclaredID {
		return nil, errors.New("program-start authority is inconsistent")
	}
	if strings.TrimSpace(run.EntrypointDeclaredID) == "" {
		return nil, errors.New("program-start entrypoint declared ID is required")
	}
	if strings.TrimSpace(deploymentVersion) == "" {
		return nil, errors.New("program-start deployment version is required")
	}

	cause, err := programStartCause(run)
	if err != nil {
		return nil, err
	}
	message := &runv0.ProgramStart{
		EntrypointDeclaredId:   run.EntrypointDeclaredID,
		RunId:                  runID,
		AttemptNumber:          uint32(attempt.Number),
		Cause:                  cause,
		DeploymentId:           deploymentID,
		DeploymentVersion:      deploymentVersion,
		WorkspaceId:            workspaceID,
		BaseWorkspaceVersionId: baseVersionID,
	}

	switch run.EntrypointKind {
	case "task":
		task, err := programStartTask(run, actor, definition)
		if err != nil {
			return nil, err
		}
		message.Entrypoint = &runv0.ProgramStart_Task{Task: task}
	case "actor":
		actorStart, err := programStartActor(run, attempt, actor)
		if err != nil {
			return nil, err
		}
		message.Entrypoint = &runv0.ProgramStart_Actor{Actor: actorStart}
	default:
		return nil, fmt.Errorf("program-start entrypoint kind %q is unsupported", run.EntrypointKind)
	}

	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode program-start frame: %w", err)
	}
	if len(body) == 0 || len(body) > frameio.MaxFrameBytes {
		return nil, fmt.Errorf(
			"program-start frame length %d is outside [1,%d]",
			len(body),
			frameio.MaxFrameBytes,
		)
	}
	var frame bytes.Buffer
	if err := frameio.WriteMessageFrame(&frame, body); err != nil {
		return nil, fmt.Errorf("frame program-start message: %w", err)
	}
	return frame.Bytes(), nil
}

func programStartTask(
	run db.Run,
	actor *db.Session,
	definition db.DeploymentDefinition,
) (*runv0.TaskStart, error) {
	if actor != nil ||
		run.SessionID.Valid ||
		run.SessionInputStartSequence.Valid ||
		run.SessionInputHighWatermark.Valid {
		return nil, errors.New("task program-start contains actor authority")
	}
	manifest, err := deployment.ParseTaskManifest(
		definition.ManifestVersion,
		definition.Manifest,
		definition.ManifestDigest,
	)
	if err != nil {
		return nil, fmt.Errorf("decode task manifest authority: %w", err)
	}
	switch manifest.Payload.Kind {
	case deployment.SchemaKindNone:
		if run.Payload != nil {
			return nil, errors.New("payload-free task run contains a payload")
		}
		return &runv0.TaskStart{
			Payload: &runv0.TaskStart_NoPayload{NoPayload: &runv0.NoPayload{}},
		}, nil
	case deployment.SchemaKindStandard:
		if run.Payload == nil {
			return nil, errors.New("payload task run has no payload")
		}
		if !json.Valid(run.Payload) {
			return nil, errors.New("task run payload is not valid JSON")
		}
		return &runv0.TaskStart{
			Payload: &runv0.TaskStart_PayloadJson{
				PayloadJson: append([]byte(nil), run.Payload...),
			},
		}, nil
	default:
		return nil, fmt.Errorf("task payload kind %q is unsupported", manifest.Payload.Kind)
	}
}

func programStartActor(
	run db.Run,
	attempt db.RunAttempt,
	actor *db.Session,
) (*runv0.ActorStart, error) {
	if actor == nil ||
		!run.SessionID.Valid ||
		actor.ID != run.SessionID ||
		actor.DeploymentDefinitionID != run.DeploymentDefinitionID ||
		actor.WorkspaceID != run.WorkspaceID ||
		actor.ActorDeclaredID != run.EntrypointDeclaredID ||
		run.Payload != nil ||
		!attempt.SessionInputStartSequence.Valid ||
		!run.SessionInputStartSequence.Valid ||
		!run.SessionInputHighWatermark.Valid ||
		attempt.SessionInputStartSequence.Int64 < 0 ||
		run.SessionInputStartSequence.Int64 < 0 ||
		attempt.SessionInputStartSequence.Int64 < run.SessionInputStartSequence.Int64 ||
		attempt.SessionInputStartSequence.Int64 > run.SessionInputHighWatermark.Int64 {
		return nil, errors.New("actor program-start authority is inconsistent")
	}
	sessionID, err := requiredClaimUUIDString("session ID", actor.ID)
	if err != nil {
		return nil, err
	}
	start := &runv0.ActorStart{
		SessionId:          sessionID,
		StartInputSequence: attempt.SessionInputStartSequence.Int64,
		InputHighWatermark: run.SessionInputHighWatermark.Int64,
	}
	if actor.Key.Valid {
		key := actor.Key.String
		start.Key = &key
	}
	return start, nil
}

func programStartCause(run db.Run) (*runv0.RunCause, error) {
	hasSchedule := run.ScheduleID.Valid ||
		run.ScheduleGeneration.Valid ||
		run.ScheduledAt.Valid ||
		run.PreviousScheduledAt.Valid ||
		run.ScheduleTimezone.Valid
	hasParent := run.ParentRunID.Valid || run.ParentOwnsLifecycle.Valid
	switch run.CauseKind {
	case "api":
		if run.EntrypointKind != "task" || hasSchedule || hasParent {
			return nil, errors.New("API run cause contains unrelated authority")
		}
		return &runv0.RunCause{Kind: &runv0.RunCause_Api{Api: &runv0.ApiCause{}}}, nil
	case "manual":
		if run.EntrypointKind != "task" || hasSchedule || hasParent {
			return nil, errors.New("manual run cause contains unrelated authority")
		}
		return &runv0.RunCause{Kind: &runv0.RunCause_Manual{Manual: &runv0.ManualCause{}}}, nil
	case "child":
		if run.EntrypointKind != "task" ||
			hasSchedule ||
			!run.ParentRunID.Valid ||
			!run.ParentOwnsLifecycle.Valid {
			return nil, errors.New("child run cause authority is incomplete")
		}
		parentID, err := requiredClaimUUIDString("parent run ID", run.ParentRunID)
		if err != nil {
			return nil, err
		}
		return &runv0.RunCause{
			Kind: &runv0.RunCause_Child{Child: &runv0.ChildCause{ParentRunId: parentID}},
		}, nil
	case "schedule":
		if run.EntrypointKind != "task" ||
			hasParent ||
			!run.ScheduleID.Valid ||
			!run.ScheduleGeneration.Valid ||
			!run.ScheduledAt.Valid ||
			!run.ScheduleTimezone.Valid ||
			strings.TrimSpace(run.ScheduleTimezone.String) == "" {
			return nil, errors.New("schedule run cause authority is incomplete")
		}
		scheduleID, err := requiredClaimUUIDString("schedule ID", run.ScheduleID)
		if err != nil {
			return nil, err
		}
		schedule := &runv0.ScheduleCause{
			ScheduleId:        scheduleID,
			ScheduledAtUnixMs: run.ScheduledAt.Time.UnixMilli(),
			Timezone:          run.ScheduleTimezone.String,
		}
		if run.PreviousScheduledAt.Valid {
			value := run.PreviousScheduledAt.Time.UnixMilli()
			schedule.PreviousScheduledAtUnixMs = &value
		}
		return &runv0.RunCause{
			Kind: &runv0.RunCause_Schedule{Schedule: schedule},
		}, nil
	case "actor_start":
		if run.EntrypointKind != "actor" || hasSchedule || hasParent {
			return nil, errors.New("actor-start run cause authority is inconsistent")
		}
		return &runv0.RunCause{
			Kind: &runv0.RunCause_ActorStart{ActorStart: &runv0.ActorStartCause{}},
		}, nil
	case "continuation":
		if run.EntrypointKind != "actor" || hasSchedule || hasParent {
			return nil, errors.New("continuation run cause authority is inconsistent")
		}
		return &runv0.RunCause{
			Kind: &runv0.RunCause_Continuation{Continuation: &runv0.ContinuationCause{}},
		}, nil
	default:
		return nil, fmt.Errorf("run cause kind %q is unsupported", run.CauseKind)
	}
}

func requiredClaimUUIDString(name string, value pgtype.UUID) (string, error) {
	id, err := pgvalue.UUIDValue(value)
	if err != nil || id == uuid.Nil {
		return "", fmt.Errorf("%s is required", name)
	}
	return id.String(), nil
}
