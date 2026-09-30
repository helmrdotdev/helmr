package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type Authority interface {
	ResolveScheduledTask(int32, string, []byte, []byte, []byte) (definition.ScheduledTaskAdmission, error)
}

type DBAdmitter struct {
	computers computer.Creator
	db        db.TxBeginner
	authority Authority
	now       func() time.Time
}

func NewDBAdmitter(database db.TxBeginner, authority Authority, ca computer.CAIssuer) (*DBAdmitter, error) {
	if database == nil {
		return nil, errors.New("schedule admission database is required")
	}
	if authority == nil {
		return nil, errors.New("schedule admission authority is required")
	}
	if ca == nil {
		return nil, errors.New("schedule Computer CA issuer is required")
	}
	return &DBAdmitter{
		computers: computer.NewCreator(ca),
		db:        database,
		authority: authority,
		now:       func() time.Time { return time.Now().UTC() },
	}, nil
}

// AdmitSchedule admits one claimed schedule instant in one transaction. An
// instant that already has a receipt commits without effects.
func (a *DBAdmitter) AdmitSchedule(ctx context.Context, candidate db.Schedule) error {
	return db.RunTx(ctx, a.db, func(tx pgx.Tx) error {
		queries := db.New(tx)

		_, err := queries.GetScheduledRunReceipt(ctx, db.GetScheduledRunReceiptParams{
			EnvironmentID: candidate.EnvironmentID,
			ScheduleID:    candidate.ID,
			ScheduledAt:   candidate.NextFireAt,
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		// A fire locks its environment, the schedule and then the schedule's
		// Secrets before it creates the Computer. The environment lock serializes
		// fires with deployment promotion, which locks the environment, the
		// scheduled Secrets and then the schedules; it is FOR NO KEY UPDATE so that
		// Computer creation, which locks its Secrets before its environment
		// reference, is not blocked by it.
		environment, err := queries.LockScheduleFireEnvironment(ctx, candidate.EnvironmentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrClaimSuperseded
		}
		if err != nil {
			return err
		}
		lockedSchedule, err := queries.LockClaimedSchedule(ctx, db.LockClaimedScheduleParams{
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
		admission, err := BuildAdmissionAt(lockedSchedule, a.now())
		if err != nil {
			return err
		}
		if !lockedSchedule.DeploymentID.Valid || !lockedSchedule.DeploymentDefinitionID.Valid {
			return &AdmissionError{Code: ErrorInvalidSchedule, Message: "schedule has no pinned task"}
		}
		task, err := queries.GetDeploymentDefinition(ctx, db.GetDeploymentDefinitionParams{
			EnvironmentID: lockedSchedule.EnvironmentID,
			DeploymentID:  lockedSchedule.DeploymentID,
			Kind:          "task",
			DeclaredID:    lockedSchedule.TaskDeclaredID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return &AdmissionError{Code: ErrorTaskNotFound, Message: "scheduled task is absent from the accepted deployment"}
		}
		if err != nil {
			return err
		}
		if task.ID != lockedSchedule.DeploymentDefinitionID {
			return &AdmissionError{Code: ErrorInvalidSchedule, Message: "schedule task does not match its pinned definition"}
		}
		program, err := queries.GetDeploymentProgramAuthority(ctx, db.GetDeploymentProgramAuthorityParams{
			EnvironmentID: lockedSchedule.EnvironmentID,
			DeploymentID:  lockedSchedule.DeploymentID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return &AdmissionError{Code: ErrorProgramUnavailable, Message: "scheduled task program is unavailable"}
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
			return &AdmissionError{Code: ErrorInvalidDefinition, Message: "scheduled task definition is invalid"}
		}

		selectedSecrets, err := computer.LockScheduleSecrets(ctx, tx,
			pgvalue.MustUUIDValue(lockedSchedule.EnvironmentID), pgvalue.MustUUIDValue(lockedSchedule.ID))
		if err != nil {
			return err
		}
		if !sameSecretPlacements(taskRun.SecretPlacements, selectedSecrets.Rows()) {
			return &AdmissionError{Code: ErrorSecretSelectionMismatch, Message: "schedule Secret selection does not match its definition"}
		}
		runID := uuid.NewV7()
		createdComputer, err := a.computers.CreateScheduled(ctx, tx, computer.ScheduledRequest{
			EnvironmentID:      pgvalue.MustUUIDValue(lockedSchedule.EnvironmentID),
			ScheduleID:         pgvalue.MustUUIDValue(lockedSchedule.ID),
			ScheduleGeneration: lockedSchedule.Generation,
			SandboxDeclaredID:  taskRun.SandboxDeclaredID,
			Secrets:            selectedSecrets,
		})
		if errors.Is(err, computer.ErrNotDeployed) {
			return &AdmissionError{Code: ErrorSandboxNotFound, Message: "schedule Sandbox is absent from its pinned deployment"}
		}
		if err != nil {
			return err
		}
		rootSpanID, err := tracing.NewSpanID()
		if err != nil {
			return err
		}
		if _, err := run.CreateTask(ctx, queries, run.TaskRequest{
			Run: db.CreateAdmittedRootTaskRunParams{
				ID:                        pgvalue.UUID(runID),
				OrgID:                     environment.OrgID,
				ProjectID:                 environment.ProjectID,
				EnvironmentID:             lockedSchedule.EnvironmentID,
				DeploymentID:              lockedSchedule.DeploymentID,
				DeploymentDefinitionID:    task.ID,
				EntrypointDeclaredID:      lockedSchedule.TaskDeclaredID,
				CauseKind:                 "schedule",
				ScheduleID:                lockedSchedule.ID,
				ScheduleGeneration:        pgtype.Int8{Int64: lockedSchedule.Generation, Valid: true},
				ScheduledAt:               lockedSchedule.NextFireAt,
				PreviousScheduledAt:       lockedSchedule.LastFireAt,
				ScheduleTimezone:          pgtype.Text{String: lockedSchedule.Timezone, Valid: true},
				ComputerID:                pgvalue.UUID(createdComputer.ID),
				BaseComputerDiskVersionID: pgvalue.UUID(createdComputer.HeadDiskVersionID),
				Payload:                   admission.Payload,
				Metadata:                  []byte(`{}`),
				Tags:                      []string{},
				QueueName:                 taskRun.QueueName,
				QueueConcurrencyLimit:     optionalInt8(taskRun.QueueConcurrencyLimit),
				Priority:                  0,
				QueuedTtlMs:               optionalInt8(taskRun.QueuedTTLMS),
				MaxActiveDurationMs:       taskRun.MaxActiveDurationMS,
				RetryPolicy:               taskRun.RetryPolicy,
				RootSpanID:                rootSpanID,
			},
			ComputerRevision: createdComputer.Revision,
		}); err != nil {
			if errors.Is(err, run.ErrSecretUnavailable) {
				return fmt.Errorf("schedule computer secret is unavailable: %w", err)
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
		return nil
	})
}

func sameSecretPlacements(
	expected []secretbinding.Placement,
	selected []db.ScheduleSecret,
) bool {
	if len(expected) != len(selected) {
		return false
	}
	actual := make(map[string]db.ScheduleSecret, len(selected))
	for _, placement := range selected {
		actual[placement.PlacementKind+"\x00"+placement.PlacementTarget] = placement
	}
	for _, placement := range expected {
		if row, ok := actual[placement.Kind+"\x00"+placement.Target]; !ok || row.Mode != placement.Mode || !slices.Equal(row.AllowedOrigins, placement.AllowedOrigins) {
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
