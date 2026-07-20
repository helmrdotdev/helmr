package schedule

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/runadmission"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type TxBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

type Authority interface {
	ResolveRuntime(string) error
	ValidateScheduledTask(int32, string, []byte, []byte, []byte) error
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

	var currentDeploymentID pgtype.UUID
	if candidate.Source == "imperative" {
		currentDeploymentID, err = queries.LockEnvironmentForScheduleAdmission(ctx, candidate.EnvironmentID)
		if err != nil {
			return err
		}
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
	if locked.Source != candidate.Source {
		return ErrClaimSuperseded
	}

	admission, err := BuildAdmissionAt(locked, a.now())
	if err != nil {
		return err
	}
	deploymentID, err := resolveDeployment(locked, currentDeploymentID)
	if err != nil {
		return err
	}
	task, err := queries.GetDeploymentDefinition(ctx, db.GetDeploymentDefinitionParams{
		EnvironmentID: locked.EnvironmentID,
		DeploymentID:  deploymentID,
		Kind:          "task",
		DeclaredID:    locked.TaskDeclaredID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return taskAuthorityError("scheduled Task is absent from the accepted Deployment")
	}
	if err != nil {
		return err
	}
	if locked.Source == "declarative" &&
		(!locked.DeclarativeDeploymentDefinitionID.Valid ||
			task.ID != locked.DeclarativeDeploymentDefinitionID) {
		return taskAuthorityError("declarative Schedule Task authority does not match its generation")
	}
	program, err := queries.GetDeploymentProgramAuthority(ctx, db.GetDeploymentProgramAuthorityParams{
		EnvironmentID: locked.EnvironmentID,
		DeploymentID:  deploymentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return taskAuthorityError("scheduled Task Program authority is unavailable")
	}
	if err != nil {
		return err
	}
	if err := a.authority.ValidateScheduledTask(
		task.ManifestVersion,
		task.DeclaredID,
		task.Manifest,
		task.ManifestDigest,
		program.QueueConfig,
	); err != nil {
		return taskAuthorityError("scheduled Task manifest authority is invalid")
	}

	workspace, err := queries.GetWorkspaceAdmissionAuthority(ctx, db.GetWorkspaceAdmissionAuthorityParams{
		EnvironmentID: locked.EnvironmentID,
		ID:            locked.WorkspaceID,
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
		return runadmission.ErrWorkspaceReservationConflict
	}
	if workspace.OwnerActorID.Valid || workspace.OwnerRunID.Valid ||
		workspace.HasActiveLease || workspace.HasActiveProcess {
		return runadmission.ErrWorkspaceReservationConflict
	}
	if !program.ProgramArchitecture.Valid ||
		!workspace.WorkspaceArchitecture.Valid ||
		program.ProgramArchitecture.String != workspace.WorkspaceArchitecture.String {
		return &AdmissionError{
			Code:    ErrorArchitectureIncompatible,
			Message: "Task Program and Workspace architectures are incompatible",
		}
	}
	runtimeDigest := "sha256:" + hex.EncodeToString(program.ProgramRuntimeDigest)
	if err := a.authority.ResolveRuntime(runtimeDigest); err != nil {
		return taskAuthorityError("scheduled Task Managed Runtime is unavailable")
	}

	runID := uuid.Must(uuid.NewV7())
	runPublicID, err := publicid.New(publicid.Run)
	if err != nil {
		return err
	}
	rootSpanID, err := tracing.NewSpanID()
	if err != nil {
		return err
	}
	if _, err := runadmission.CreateTask(ctx, queries, runadmission.TaskRequest{
		Run: db.CreateAdmittedRootTaskRunParams{
			ID:                     pgvalue.UUID(runID),
			PublicID:               runPublicID,
			OrgID:                  locked.OrgID,
			ProjectID:              locked.ProjectID,
			EnvironmentID:          locked.EnvironmentID,
			DeploymentID:           deploymentID,
			DeploymentDefinitionID: task.ID,
			EntrypointDeclaredID:   locked.TaskDeclaredID,
			CauseKind:              "schedule",
			ScheduleID:             locked.ID,
			ScheduleGeneration:     pgtype.Int8{Int64: locked.Generation, Valid: true},
			ScheduledAt:            locked.NextFireAt,
			PreviousScheduledAt:    locked.LastFireAt,
			ScheduleTimezone:       pgtype.Text{String: locked.Timezone, Valid: true},
			WorkspaceID:            locked.WorkspaceID,
			BaseWorkspaceVersionID: workspace.HeadVersionID,
			Payload:                admission.Payload,
			Metadata:               locked.RunMetadata,
			Tags:                   locked.RunTags,
			QueueName:              locked.QueueName,
			ConcurrencyKey:         locked.ConcurrencyKey,
			QueueConcurrencyLimit:  locked.QueueConcurrencyLimit,
			Priority:               locked.Priority,
			QueuedTtlMs:            locked.QueuedTtlMs,
			MaxActiveDurationMs:    locked.MaxActiveDurationMs,
			RetryPolicy:            locked.RetryPolicy,
			RootSpanID:             rootSpanID,
		},
		WorkspaceStateVersion: workspace.StateVersion,
	}); err != nil {
		if errors.Is(err, runadmission.ErrSecretUnavailable) {
			return workspaceError("Schedule Workspace Secret is unavailable")
		}
		return err
	}

	if _, err := queries.AdvanceScheduleCursor(ctx, db.AdvanceScheduleCursorParams{
		ExpectedScheduledAt: locked.NextFireAt,
		NextFireAt:          pgvalue.TimestamptzUTCZeroInvalid(admission.NextFireAt),
		EnvironmentID:       locked.EnvironmentID,
		ID:                  locked.ID,
		ExpectedGeneration:  locked.Generation,
		ClaimedBy:           locked.ClaimedBy,
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

func resolveDeployment(value db.Schedule, currentDeploymentID pgtype.UUID) (pgtype.UUID, error) {
	if value.Source == "declarative" {
		if !value.DeclarativeDeploymentID.Valid {
			return pgtype.UUID{}, taskAuthorityError("declarative Schedule has no pinned Deployment")
		}
		return value.DeclarativeDeploymentID, nil
	}
	if value.Source != "imperative" {
		return pgtype.UUID{}, &AdmissionError{
			Code:    ErrorGenerationInvalid,
			Message: fmt.Sprintf("unsupported Schedule source %q", value.Source),
		}
	}
	if !currentDeploymentID.Valid {
		return pgtype.UUID{}, taskAuthorityError("Environment has no promoted Deployment")
	}
	return currentDeploymentID, nil
}

func taskAuthorityError(message string) error {
	return &AdmissionError{Code: ErrorTaskAuthorityInvalid, Message: message}
}

func workspaceError(message string) error {
	return &AdmissionError{Code: ErrorWorkspaceUnavailable, Message: message}
}
