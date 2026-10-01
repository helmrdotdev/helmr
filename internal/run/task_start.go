package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	// ErrTaskStartInvalid reports a Task start the caller described
	// incorrectly.
	ErrTaskStartInvalid = errors.New("task start request is invalid")
	// ErrTaskNotDeployed reports a Task the deployment does not declare.
	ErrTaskNotDeployed = errors.New("task declaration is not deployed")
	// ErrTaskComputerNotFound reports a child Task's Computer that the
	// invoking execution cannot address.
	ErrTaskComputerNotFound = errors.New("task start computer was not found")
	// ErrTaskComputerUnavailable reports a Computer that does not admit the
	// new Task Run.
	ErrTaskComputerUnavailable = errors.New("task start computer cannot accept execution")
	// ErrTaskSecretUnavailable reports a Computer Secret the new Task Run
	// cannot resolve.
	ErrTaskSecretUnavailable = errors.New("task start computer secret is unavailable")
	// ErrTaskStartAuthority reports a deployment that cannot admit the Task
	// Run.
	ErrTaskStartAuthority = errors.New("task start authority is unavailable")
	// ErrTaskStartReceiptInvalid reports an idempotency claim whose recorded
	// receipt cannot be replayed.
	ErrTaskStartReceiptInvalid = errors.New("task start idempotency receipt is invalid")
	// ErrTaskPayloadPresenceInvalid reports a payload whose presence does not
	// match the Task's declaration.
	ErrTaskPayloadPresenceInvalid = errors.New("task payload presence does not match its declaration")
	// ErrComputerPreparationExhausted reports a Computer whose preparation
	// limit was reached.
	ErrComputerPreparationExhausted = errors.New("computer preparation limit reached")
)

// TaskStart is a normalized request to start a root Task Run on a Computer
// from the environment's current deployment.
type TaskStart struct {
	OrgID          uuid.UUID
	ProjectID      uuid.UUID
	EnvironmentID  uuid.UUID
	TaskDeclaredID string
	PayloadPresent bool
	Payload        json.RawMessage
	ComputerID     uuid.UUID
	QueueName      string
	ConcurrencyKey *string
	Priority       int32
	QueuedTTLMS    *int64
	RetryPolicy    json.RawMessage
	Metadata       json.RawMessage
	Tags           []string
	// Claim makes the start replayable; a nil Claim starts without one.
	Claim idempotency.Request
}

// TaskStarted is the Run a Task start admitted, or replayed from its
// idempotency claim.
type TaskStarted struct {
	RunID    uuid.UUID
	Replayed bool
}

type taskStartReceipt struct {
	RunID string `json:"run_id"`
}

// StartTask admits a root Task Run in its own transaction. It acquires the
// start's idempotency claim first and replays a completed claim's receipt
// {"run_id"}. A new start then locks the environment's current deployment of
// the Task, the Computer's Secrets and then the Computer through the computer
// owner's admission lock, creates the Run and its first Attempt, records the
// Computer admission and the Attempt's Secret resolutions, and completes the
// claim.
func StartTask(ctx context.Context, txb db.TxBeginner, start TaskStart) (TaskStarted, error) {
	var started TaskStarted
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		var claim *db.IdempotencyClaim
		if start.Claim != nil {
			claims, err := idempotency.TransactionFor(tx)
			if err != nil {
				return err
			}
			acquired, err := claims.Acquire(ctx, start.Claim)
			if err != nil {
				return err
			}
			if acquired.Claim.Status == "completed" {
				replayed, err := taskStartedFromReceipt(acquired.Claim.Receipt)
				if err != nil {
					return err
				}
				replayed.Replayed = true
				started = replayed
				return nil
			}
			if acquired.Claim.Status != "pending" {
				return ErrTaskStartReceiptInvalid
			}
			claim = &acquired.Claim
		}

		program, err := q.LockTaskStartDeploymentAuthority(ctx, db.LockTaskStartDeploymentAuthorityParams{
			TaskDeclaredID: start.TaskDeclaredID,
			OrgID:          pgvalue.UUID(start.OrgID),
			ProjectID:      pgvalue.UUID(start.ProjectID),
			EnvironmentID:  pgvalue.UUID(start.EnvironmentID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTaskNotDeployed
		}
		if err != nil {
			return fmt.Errorf("lock task start deployment authority: %w", err)
		}
		admission, err := definition.ResolveTaskRunAdmission(
			program.TaskManifestVersion,
			start.TaskDeclaredID,
			program.TaskManifest,
			program.TaskManifestDigest,
			program.QueueConfig,
			start.QueueName,
			start.QueuedTTLMS,
			start.RetryPolicy,
		)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrTaskStartAuthority, err)
		}
		if admission.HasPayload != start.PayloadPresent {
			return ErrTaskPayloadPresenceInvalid
		}

		bindings, err := q.LockComputerSecretsForAdmission(ctx, pgvalue.UUID(start.ComputerID))
		if err != nil {
			return fmt.Errorf("lock task start computer secrets: %w", err)
		}
		if !admissionSecretsAvailable(bindings) {
			return ErrTaskSecretUnavailable
		}
		locked, err := computer.LockForAdmission(ctx, tx, start.EnvironmentID, start.ComputerID)
		if errors.Is(err, computer.ErrNotFound) {
			return ErrTaskComputerUnavailable
		}
		if err != nil {
			return err
		}
		admitted := locked.Row()
		if admitted.OrgID != pgvalue.UUID(start.OrgID) ||
			admitted.ProjectID != pgvalue.UUID(start.ProjectID) ||
			admitted.Status != db.ComputerStatusActive ||
			(admitted.DesiredState != db.ComputerDesiredStateActive &&
				admitted.DesiredState != db.ComputerDesiredStateStopped) ||
			admitted.DirtyState == db.ComputerDirtyStateCaptureFailed ||
			admitted.DirtyState == db.ComputerDirtyStateDirtyStateLost ||
			!admitted.HeadDiskVersionID.Valid {
			return ErrTaskComputerUnavailable
		}
		if len(admitted.PreparationFailure) > 0 {
			return ErrComputerPreparationExhausted
		}
		canAdmit, err := computer.CanAdmitProgram(ctx, q, admitted.EnvironmentID, admitted.ID,
			admitted.ComputerSpecID, program.DeploymentID)
		if err != nil {
			return err
		}
		if !canAdmit {
			return ErrTaskComputerUnavailable
		}
		runID := uuid.NewV7()
		rootSpanID, err := tracing.NewSpanID()
		if err != nil {
			return err
		}
		claimID := pgtype.UUID{}
		if claim != nil {
			claimID = claim.ID
		}
		admissionTime, err := q.GetRunAdmissionTime(ctx)
		if err != nil {
			return fmt.Errorf("get task run admission time: %w", err)
		}
		now := admissionTime.Time.UTC()
		queuedExpiresAt := pgtype.Timestamptz{}
		if admission.QueuedTTLMS != nil {
			queuedExpiresAt = pgvalue.Timestamptz(now.Add(
				time.Duration(*admission.QueuedTTLMS) * time.Millisecond,
			))
		}
		created, err := q.CreateRootRunFromCurrentDeployment(ctx, db.CreateRootRunFromCurrentDeploymentParams{
			EntrypointDeclaredID: start.TaskDeclaredID,
			ComputerID:           admitted.ID,
			OrgID:                pgvalue.UUID(start.OrgID), ProjectID: pgvalue.UUID(start.ProjectID),
			BaseComputerDiskVersionID: admitted.HeadDiskVersionID,
			EnvironmentID:             pgvalue.UUID(start.EnvironmentID), ClaimID: claimID,
			ID: pgvalue.UUID(runID), CauseKind: "api",
			Payload: start.Payload, Metadata: start.Metadata, Tags: start.Tags,
			QueueName: admission.QueueName, ConcurrencyKey: pgvalue.TextPtr(start.ConcurrencyKey),
			QueueConcurrencyLimit: optionalInt8(admission.QueueConcurrencyLimit),
			Priority:              start.Priority,
			QueueOriginAt:         pgvalue.Timestamptz(now),
			QueueScoreAt:          pgvalue.Timestamptz(now.Add(-time.Duration(start.Priority) * time.Second)),
			QueuedExpiresAt:       queuedExpiresAt,
			MaxActiveDurationMs:   admission.MaxActiveDurationMS,
			RetryPolicy:           admission.RetryPolicy, RootSpanID: rootSpanID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTaskStartAuthority
		}
		if err != nil {
			return fmt.Errorf("create task run: %w", err)
		}
		if err := locked.Touch(ctx); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrTaskComputerUnavailable
			}
			return fmt.Errorf("record task computer admission: %w", err)
		}
		if err := secret.CreateAttemptResolutions(
			ctx, q, admitted.ID, created.ID, 1, admissionSecretResolutions(bindings),
		); err != nil {
			return fmt.Errorf("record task run secret resolutions: %w", err)
		}
		started = TaskStarted{RunID: runID}
		if claim == nil {
			return nil
		}
		receipt, err := json.Marshal(taskStartReceipt{RunID: runID.String()})
		if err != nil {
			return err
		}
		claims, err := idempotency.TransactionFor(tx)
		if err != nil {
			return err
		}
		_, err = claims.Complete(ctx, *claim, receipt)
		return err
	})
	if err != nil {
		return TaskStarted{}, err
	}
	return started, nil
}

func taskStartedFromReceipt(raw []byte) (TaskStarted, error) {
	var receipt taskStartReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return TaskStarted{}, ErrTaskStartReceiptInvalid
	}
	runID, err := ids.Parse(receipt.RunID)
	if err != nil {
		return TaskStarted{}, ErrTaskStartReceiptInvalid
	}
	return TaskStarted{RunID: runID}, nil
}

// admissionSecretsAvailable reports whether every Secret bound to an
// admitted Computer is active with a current version.
func admissionSecretsAvailable(bindings []db.LockComputerSecretsForAdmissionRow) bool {
	for _, binding := range bindings {
		if binding.SecretStatus != "active" || !binding.CurrentVersionID.Valid {
			return false
		}
	}
	return true
}

// admissionSecretResolutions are the Secret resolutions a new Run's first
// Attempt records from its Computer's locked bindings.
func admissionSecretResolutions(bindings []db.LockComputerSecretsForAdmissionRow) []secret.Resolution {
	resolutions := make([]secret.Resolution, len(bindings))
	for index, binding := range bindings {
		resolutions[index] = secret.Resolution{
			PlacementKind: binding.PlacementKind, PlacementTarget: binding.PlacementTarget,
			SecretID: binding.SecretID, SecretVersionID: binding.CurrentVersionID,
			RevocationGeneration: binding.RevocationGeneration,
		}
	}
	return resolutions
}

func optionalInt8(value *int64) pgtype.Int8 {
	if value == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *value, Valid: true}
}
