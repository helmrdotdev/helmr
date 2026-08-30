package schedule

import (
	"context"
	"errors"
	"fmt"
	"time"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/helmrdotdev/helmr/internal/workspace"
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
	SandboxDeclaredID     string
	SecretPlacements      []workspace.SecretPlacement
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
		return taskAuthorityError("schedule has no pinned task authority")
	}
	task, err := queries.GetDeploymentDefinition(ctx, db.GetDeploymentDefinitionParams{
		EnvironmentID: lockedSchedule.EnvironmentID,
		DeploymentID:  lockedSchedule.DeploymentID,
		Kind:          "task",
		DeclaredID:    lockedSchedule.TaskDeclaredID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return taskAuthorityError("scheduled task is absent from the accepted deployment")
	}
	if err != nil {
		return err
	}
	if task.ID != lockedSchedule.DeploymentDefinitionID {
		return taskAuthorityError("schedule task authority does not match its generation")
	}
	program, err := queries.GetDeploymentProgramAuthority(ctx, db.GetDeploymentProgramAuthorityParams{
		EnvironmentID: lockedSchedule.EnvironmentID,
		DeploymentID:  lockedSchedule.DeploymentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return taskAuthorityError("scheduled task program authority is unavailable")
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
		return taskAuthorityError("scheduled task manifest authority is invalid")
	}

	selectedSecrets, err := queries.ListScheduleSecrets(ctx, db.ListScheduleSecretsParams{
		EnvironmentID: lockedSchedule.EnvironmentID,
		ScheduleID:    lockedSchedule.ID,
	})
	if err != nil {
		return err
	}
	if !sameSecretPlacements(taskRun.SecretPlacements, selectedSecrets) {
		return taskAuthorityError("schedule Secret selection does not match its generation")
	}
	runID := uuid.NewV7()
	workspaceID := uuid.NewV7()
	initialVersionID := uuid.NewV7()
	createdWorkspace, err := queries.CreateWorkspaceForScheduleFire(
		ctx,
		db.CreateWorkspaceForScheduleFireParams{
			SandboxDeclaredID:  taskRun.SandboxDeclaredID,
			EnvironmentID:      lockedSchedule.EnvironmentID,
			ScheduleID:         lockedSchedule.ID,
			ExpectedGeneration: lockedSchedule.Generation,
			ID:                 pgvalue.UUID(workspaceID),
			InitialVersionID:   pgvalue.UUID(initialVersionID),
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return sandboxAuthorityError("schedule Sandbox is absent from its pinned deployment")
	}
	if err != nil {
		return err
	}
	for _, selected := range selectedSecrets {
		if _, err := queries.CreateWorkspaceSecret(ctx, db.CreateWorkspaceSecretParams{
			WorkspaceID:     createdWorkspace.ID,
			EnvironmentID:   lockedSchedule.EnvironmentID,
			PlacementKind:   selected.PlacementKind,
			PlacementTarget: selected.PlacementTarget,
			SecretID:        selected.SecretID,
		}); err != nil {
			return err
		}
	}
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
			WorkspaceID:            createdWorkspace.ID,
			BaseWorkspaceVersionID: createdWorkspace.HeadVersionID,
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
		WorkspaceStateVersion: createdWorkspace.StateVersion,
	}); err != nil {
		if errors.Is(err, run.ErrSecretUnavailable) {
			return fmt.Errorf("schedule workspace secret is unavailable: %w", err)
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
		return fmt.Errorf("commit schedule run admission: %w", err)
	}
	return nil
}

func taskAuthorityError(message string) error {
	return &AdmissionError{Code: ErrorTaskAuthorityInvalid, Message: message}
}

func sandboxAuthorityError(message string) error {
	return &AdmissionError{Code: ErrorSandboxAuthorityInvalid, Message: message}
}

func sameSecretPlacements(
	expected []workspace.SecretPlacement,
	selected []db.ScheduleSecret,
) bool {
	if len(expected) != len(selected) {
		return false
	}
	actual := make(map[string]struct{}, len(selected))
	for _, placement := range selected {
		actual[placement.PlacementKind+"\x00"+placement.PlacementTarget] = struct{}{}
	}
	for _, placement := range expected {
		if _, ok := actual[placement.Kind+"\x00"+placement.Target]; !ok {
			return false
		}
	}
	return true
}

func optionalInt8(value *int64) pgtype.Int8 {
	if value == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *value, Valid: true}
}
