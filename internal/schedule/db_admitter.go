package schedule

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type TxBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

type Authority interface {
	ResolveScheduledTask(int32, string, []byte, []byte, []byte) (TaskRun, error)
}

type TaskRun struct {
	QueueName             string
	QueueConcurrencyLimit *int64
	QueuedTTLMS           *int64
	MaxActiveDurationMS   int64
	RetryPolicy           []byte
}

type DBAdmitter struct {
	db        TxBeginner
	authority Authority
	now       func() time.Time
}

func NewDBAdmitter(database TxBeginner, authority Authority) (*DBAdmitter, error) {
	if database == nil {
		return nil, errors.New("schedule admission database is required")
	}
	if authority == nil {
		return nil, errors.New("schedule admission authority is required")
	}
	return &DBAdmitter{
		db:        database,
		authority: authority,
		now:       func() time.Time { return time.Now().UTC() },
	}, nil
}

func (a *DBAdmitter) AdmitSchedule(ctx context.Context, candidate db.Schedule) error {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	queries := db.New(tx)

	_, err = queries.GetScheduledRunReceipt(ctx, db.GetScheduledRunReceiptParams{
		EnvironmentID: candidate.EnvironmentID,
		ScheduleID:    candidate.ID,
		ScheduledAt:   candidate.NextFireAt,
	})
	if err == nil {
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	locked, err := queries.LockClaimedSchedule(ctx, db.LockClaimedScheduleParams{
		EnvironmentID:       candidate.EnvironmentID,
		ID:                  candidate.ID,
		ExpectedGeneration:  candidate.Generation,
		ExpectedScheduledAt: candidate.NextFireAt,
		ClaimedBy:           candidate.ClaimedBy,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrClaimSuperseded
	}
	if err != nil {
		return err
	}
	lockedSchedule := locked.Schedule
	admission, err := BuildAdmissionAt(lockedSchedule, a.now())
	if err != nil {
		return err
	}
	if !lockedSchedule.DeploymentID.Valid || !lockedSchedule.DeploymentDefinitionID.Valid {
		return taskAuthorityError("Schedule has no pinned Task authority")
	}
	task, err := queries.GetDeploymentDefinition(ctx, db.GetDeploymentDefinitionParams{
		EnvironmentID: lockedSchedule.EnvironmentID,
		DeploymentID:  lockedSchedule.DeploymentID,
		Kind:          "task",
		DeclaredID:    lockedSchedule.TaskDeclaredID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return taskAuthorityError("scheduled Task is absent from the accepted Deployment")
	}
	if err != nil {
		return err
	}
	if task.ID != lockedSchedule.DeploymentDefinitionID {
		return taskAuthorityError("Schedule Task authority does not match its generation")
	}
	program, err := queries.GetDeploymentProgramAuthority(ctx, db.GetDeploymentProgramAuthorityParams{
		EnvironmentID: lockedSchedule.EnvironmentID,
		DeploymentID:  lockedSchedule.DeploymentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return taskAuthorityError("scheduled Task Program authority is unavailable")
	}
	if err != nil {
		return err
	}
	taskRun, err := a.authority.ResolveScheduledTask(
		task.ManifestVersion,
		task.DeclaredID,
		task.Manifest,
		task.ManifestDigest,
		program.QueueConfig,
	)
	if err != nil {
		return taskAuthorityError("scheduled Task manifest authority is invalid")
	}

	workspace, err := queries.LockWorkspaceAdmissionAuthority(ctx, db.LockWorkspaceAdmissionAuthorityParams{
		EnvironmentID: lockedSchedule.EnvironmentID,
		ID:            lockedSchedule.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return workspaceError("Schedule Workspace is unavailable")
	}
	if err != nil {
		return err
	}
	if workspace.State != db.WorkspaceStateActive ||
		(workspace.DesiredState != db.WorkspaceDesiredStateActive &&
			workspace.DesiredState != db.WorkspaceDesiredStateStopped) ||
		!workspace.HeadVersionID.Valid {
		return workspaceError("Schedule Workspace cannot accept execution")
	}
	if workspace.DirtyState != db.WorkspaceDirtyStateClean {
		return run.ErrWorkspaceReservationConflict
	}
	if workspace.OwnerActorID.Valid || workspace.OwnerRunID.Valid ||
		workspace.HasActiveLease || workspace.HasActiveProcess {
		return run.ErrWorkspaceReservationConflict
	}
	runID := uuid.Must(uuid.NewV7())
	rootSpanID, err := tracing.NewSpanID()
	if err != nil {
		return err
	}
	if _, err := run.CreateTask(ctx, queries, run.TaskRequest{
		Run: db.CreateAdmittedRootTaskRunParams{
			ID:                     pgvalue.UUID(runID),
			OrgID:                  locked.OrgID,
			ProjectID:              locked.ProjectID,
			EnvironmentID:          lockedSchedule.EnvironmentID,
			DeploymentID:           lockedSchedule.DeploymentID,
			DeploymentDefinitionID: task.ID,
			EntrypointDeclaredID:   lockedSchedule.TaskDeclaredID,
			CauseKind:              "schedule",
			ScheduleID:             lockedSchedule.ID,
			ScheduleGeneration:     pgtype.Int8{Int64: lockedSchedule.Generation, Valid: true},
			ScheduledAt:            lockedSchedule.NextFireAt,
			PreviousScheduledAt:    lockedSchedule.LastFireAt,
			ScheduleTimezone:       pgtype.Text{String: lockedSchedule.Timezone, Valid: true},
			WorkspaceID:            lockedSchedule.WorkspaceID,
			BaseWorkspaceVersionID: workspace.HeadVersionID,
			Payload:                admission.Payload,
			Metadata:               []byte(`{}`),
			Tags:                   []string{},
			QueueName:              taskRun.QueueName,
			QueueConcurrencyLimit:  optionalInt8(taskRun.QueueConcurrencyLimit),
			Priority:               0,
			QueuedTtlMs:            optionalInt8(taskRun.QueuedTTLMS),
			MaxActiveDurationMs:    taskRun.MaxActiveDurationMS,
			RetryPolicy:            taskRun.RetryPolicy,
			RootSpanID:             rootSpanID,
		},
		WorkspaceStateVersion: workspace.StateVersion,
	}); err != nil {
		if errors.Is(err, run.ErrSecretUnavailable) {
			return workspaceError("Schedule Workspace Secret is unavailable")
		}
		return err
	}

	if _, err := queries.AdvanceScheduleCursor(ctx, db.AdvanceScheduleCursorParams{
		ExpectedScheduledAt: lockedSchedule.NextFireAt,
		NextFireAt:          pgvalue.TimestamptzUTCZeroInvalid(admission.NextFireAt),
		EnvironmentID:       lockedSchedule.EnvironmentID,
		ID:                  lockedSchedule.ID,
		ExpectedGeneration:  lockedSchedule.Generation,
		ClaimedBy:           lockedSchedule.ClaimedBy,
	}); errors.Is(err, pgx.ErrNoRows) {
		return ErrClaimSuperseded
	} else if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Schedule Run admission: %w", err)
	}
	return nil
}

func taskAuthorityError(message string) error {
	return &AdmissionError{Code: ErrorTaskAuthorityInvalid, Message: message}
}

func workspaceError(message string) error {
	return &AdmissionError{Code: ErrorWorkspaceUnavailable, Message: message}
}

func optionalInt8(value *int64) pgtype.Int8 {
	if value == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *value, Valid: true}
}
