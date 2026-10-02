package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	// ErrStartInvalid reports an Actor start the caller described
	// incorrectly.
	ErrStartInvalid = errors.New("actor start request is invalid")
	// ErrActorNotDeployed reports an Actor the environment's current
	// deployment does not declare.
	ErrActorNotDeployed = errors.New("actor declaration is not deployed")
	// ErrStartComputerNotFound reports a run-sourced start Computer that the
	// source execution cannot address.
	ErrStartComputerNotFound = errors.New("actor start computer was not found")
	// ErrStartAuthority reports a deployment that cannot admit the Actor's
	// boot Run.
	ErrStartAuthority = errors.New("actor start authority is unavailable")
	// ErrStartComputerUnavailable reports a Computer that does not admit the
	// new Session.
	ErrStartComputerUnavailable = errors.New("actor start computer cannot accept execution")
	// ErrStartSecretUnavailable reports a Computer Secret the boot Run cannot
	// resolve.
	ErrStartSecretUnavailable = errors.New("actor start computer secret is unavailable")
	// ErrStartReceiptInvalid reports an idempotency claim whose recorded
	// receipt cannot be replayed.
	ErrStartReceiptInvalid = errors.New("actor start idempotency receipt is invalid")
)

// KeyConflictError reports an Actor key that already names another Session.
type KeyConflictError struct {
	Key string
}

func (e KeyConflictError) Error() string {
	return fmt.Sprintf("actor key %q already belongs to another actor", e.Key)
}

// StartRequest is a normalized request to start an Actor's Session on a
// Computer from the environment's current deployment. Empty queue, TTL and
// retry options take the Actor declaration's.
type StartRequest struct {
	OrgID           uuid.UUID
	ProjectID       uuid.UUID
	EnvironmentID   uuid.UUID
	ActorDeclaredID string
	ComputerID      uuid.UUID
	Key             *string
	QueueName       string
	ConcurrencyKey  *string
	Priority        int32
	QueuedTTLMS     *int64
	RetryPolicy     json.RawMessage
	Metadata        json.RawMessage
	Tags            []string
}

// Started is the Session and boot Run an Actor start admitted, or replayed
// from its idempotency claim.
type Started struct {
	SessionID uuid.UUID
	BootRunID uuid.UUID
	Replayed  bool
}

type startReceipt struct {
	SessionID string `json:"sessionId"`
	BootRunID string `json:"bootRunId"`
}

// Start admits an Actor's Session and boot Run in its own transaction. A
// non-nil claim makes the start replayable: Start acquires it first and
// replays a completed claim's receipt {"sessionId","bootRunId"}. A new start
// then locks the environment FOR NO KEY UPDATE with the current deployment's
// Actor, takes the transaction advisory lock on the environment, Actor and
// key and checks the key is free, locks the Computer's Secrets and then the
// Computer through the computer owner's admission lock, creates the Session
// and its boot Run, records the Computer admission and the boot Attempt's
// Secret resolutions, and completes the claim.
func Start(ctx context.Context, txb db.TxBeginner, claimRequest idempotency.Request, request StartRequest) (Started, error) {
	return start(ctx, txb, claimRequest, request, nil)
}

// StartFromRun is Start for a worker acting from a live source Run. In the
// same transaction it authorizes the source with the start Computer
// addressed: it locks the source Computer's and the start Computer's
// Secrets, the execution fence with the start Computer and the source
// attempt's delivery Secrets, and requires the source's Session, if any, to
// be neither held nor settling its active Turn. A replay authorizes right
// after the claim; a new start authorizes after the key check and before
// the start Computer's Secrets, which it then locks again without comparing
// them with the first lock. A start Computer outside the source's
// environment is ErrStartComputerNotFound; a missing or mismatched source is
// run.ErrStaleSource.
func StartFromRun(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, claimRequest idempotency.Request, request StartRequest) (Started, error) {
	return start(ctx, txb, claimRequest, request, &fence)
}

func start(ctx context.Context, txb db.TxBeginner, claimRequest idempotency.Request, request StartRequest, source *run.ExecutionFence) (Started, error) {
	authorize := func(tx pgx.Tx) error {
		if source == nil {
			return nil
		}
		return authorizeStartSource(ctx, tx, *source, pgvalue.UUID(request.ComputerID))
	}
	var started Started
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		var claim *db.IdempotencyClaim
		if claimRequest != nil {
			claims, err := idempotency.TransactionFor(tx)
			if err != nil {
				return err
			}
			acquired, err := claims.Acquire(ctx, claimRequest)
			if err != nil {
				return err
			}
			if acquired.Claim.Status == "completed" {
				if err := authorize(tx); err != nil {
					return err
				}
				replayed, err := startedFromReceipt(acquired.Claim.Receipt)
				if err != nil {
					return err
				}
				replayed.Replayed = true
				started = replayed
				return nil
			}
			if acquired.Claim.Status != "pending" {
				return ErrStartReceiptInvalid
			}
			claim = &acquired.Claim
		}

		program, err := q.LockActorStartDeploymentAuthority(ctx, db.LockActorStartDeploymentAuthorityParams{
			ActorDeclaredID: request.ActorDeclaredID,
			OrgID:           pgvalue.UUID(request.OrgID),
			ProjectID:       pgvalue.UUID(request.ProjectID),
			EnvironmentID:   pgvalue.UUID(request.EnvironmentID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrActorNotDeployed
		}
		if err != nil {
			return fmt.Errorf("lock actor start deployment authority: %w", err)
		}
		computerID := pgvalue.UUID(request.ComputerID)

		if request.Key != nil {
			if err := q.LockActorStartKey(ctx, db.LockActorStartKeyParams{
				EnvironmentID:   pgvalue.UUID(request.EnvironmentID),
				ActorDeclaredID: request.ActorDeclaredID,
				Key:             *request.Key,
			}); err != nil {
				return fmt.Errorf("lock actor start key: %w", err)
			}
			_, err := q.GetSessionByKey(ctx, db.GetSessionByKeyParams{
				EnvironmentID:   pgvalue.UUID(request.EnvironmentID),
				ActorDeclaredID: request.ActorDeclaredID,
				Key:             pgvalue.Text(*request.Key),
			})
			if err == nil {
				return KeyConflictError{Key: *request.Key}
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("check actor start key: %w", err)
			}
		}

		if err := authorize(tx); err != nil {
			return err
		}
		bindings, err := q.LockComputerSecretsForAdmission(ctx, computerID)
		if err != nil {
			return fmt.Errorf("lock actor start computer secrets: %w", err)
		}
		admission, err := computer.LockForAdmission(ctx, tx, request.EnvironmentID, request.ComputerID)
		if errors.Is(err, computer.ErrNotFound) {
			return ErrStartComputerUnavailable
		}
		if err != nil {
			return err
		}
		authority := admission.Row()
		if authority.OrgID != pgvalue.UUID(request.OrgID) ||
			authority.ProjectID != pgvalue.UUID(request.ProjectID) ||
			authority.Status != db.ComputerStatusActive ||
			(authority.DesiredState != db.ComputerDesiredStateActive &&
				authority.DesiredState != db.ComputerDesiredStateStopped) ||
			authority.DirtyState == db.ComputerDirtyStateDirtyStateLost ||
			!authority.HeadDiskVersionID.Valid {
			return ErrStartComputerUnavailable
		}
		if len(authority.PreparationFailure) > 0 {
			return computer.ErrPreparationExhausted
		}
		canAdmit, err := computer.CanAdmitProgram(ctx, q, authority.EnvironmentID, authority.ID,
			authority.ComputerSpecID, program.DeploymentID)
		if err != nil {
			return err
		}
		if !canAdmit {
			return ErrStartComputerUnavailable
		}
		for _, binding := range bindings {
			if binding.SecretStatus != "active" || !binding.CurrentVersionID.Valid {
				return ErrStartSecretUnavailable
			}
		}
		runAuthority, err := definition.ResolveActorRunAdmission(
			program.ActorManifestVersion,
			request.ActorDeclaredID,
			program.ActorManifest,
			program.ActorManifestDigest,
			program.QueueConfig,
			request.QueueName,
		)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrStartAuthority, err)
		}
		queuedTTL := request.QueuedTTLMS
		if queuedTTL == nil {
			queuedTTL = runAuthority.QueuedTTLMS
		}
		retryPolicy := request.RetryPolicy
		if len(retryPolicy) == 0 {
			retryPolicy = runAuthority.RetryPolicy
		}
		sessionID := uuid.NewV7()
		runID := uuid.NewV7()
		rootSpanID, err := tracing.NewSpanID()
		if err != nil {
			return err
		}
		claimID := pgtype.UUID{}
		if claim != nil {
			claimID = claim.ID
		}
		_, err = q.CreateSession(ctx, db.CreateSessionParams{
			ID:    pgvalue.UUID(sessionID),
			OrgID: pgvalue.UUID(request.OrgID), ProjectID: pgvalue.UUID(request.ProjectID),
			Key: pgvalue.TextPtr(request.Key), RunQueueName: runAuthority.QueueName,
			RunConcurrencyKey:        pgvalue.TextPtr(request.ConcurrencyKey),
			RunQueueConcurrencyLimit: int8Ptr(runAuthority.QueueConcurrencyLimit),
			RunPriority:              request.Priority, RunQueueTtlMs: int8Ptr(queuedTTL),
			RunMaxActiveDurationMs: runAuthority.MaxActiveDurationMS,
			RunRetryPolicy:         retryPolicy,
			RunMetadata:            request.Metadata, RunTags: request.Tags,
			ComputerID: authority.ID, EnvironmentID: pgvalue.UUID(request.EnvironmentID),
			DeploymentDefinitionID: program.ActorDefinitionID, ActorDeclaredID: request.ActorDeclaredID,
		})
		if err != nil {
			var postgresError *pgconn.PgError
			if errors.As(err, &postgresError) &&
				postgresError.ConstraintName == "sessions_environment_declared_id_key_uidx" {
				return KeyConflictError{Key: stringPtrValue(request.Key)}
			}
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrStartAuthority
			}
			return fmt.Errorf("create session: %w", err)
		}

		bootRun, err := q.CreateActorStartRun(ctx, db.CreateActorStartRunParams{
			EnvironmentID: pgvalue.UUID(request.EnvironmentID), SessionID: pgvalue.UUID(sessionID),
			ComputerID: authority.ID, ClaimID: claimID,
			ID:                        pgvalue.UUID(runID),
			BaseComputerDiskVersionID: authority.HeadDiskVersionID,
			InputHighWatermark:        pgtype.Int8{Int64: 0, Valid: true},
			RootSpanID:                rootSpanID,
		})
		if err != nil {
			return fmt.Errorf("create actor boot run: %w", err)
		}
		if _, err := q.SetSessionCurrentRun(ctx, db.SetSessionCurrentRunParams{
			RunID: bootRun.ID, EnvironmentID: bootRun.EnvironmentID,
			ID: pgvalue.UUID(sessionID), ComputerID: authority.ID,
		}); err != nil {
			return fmt.Errorf("install actor boot run: %w", err)
		}
		if err := admission.Touch(ctx); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrStartComputerUnavailable
			}
			return fmt.Errorf("record actor computer admission: %w", err)
		}
		if err := secret.CreateAttemptResolutions(
			ctx, q, authority.ID, bootRun.ID, 1, run.SecretResolutions(bindings),
		); err != nil {
			return fmt.Errorf("record actor boot run secret resolutions: %w", err)
		}
		started = Started{SessionID: sessionID, BootRunID: runID}
		if claim != nil {
			receipt, err := json.Marshal(startReceipt{SessionID: sessionID.String(), BootRunID: runID.String()})
			if err != nil {
				return err
			}
			claims, err := idempotency.TransactionFor(tx)
			if err != nil {
				return err
			}
			if _, err := claims.Complete(ctx, *claim, receipt); err != nil {
				return err
			}
		}
		return nil
	})
	return started, err
}

// authorizeStartSource locks the source Computer's and the start Computer's
// Secrets, the execution fence with the start Computer and the source
// attempt's delivery Secrets, then checks the source Session as
// CheckSourceSession does.
func authorizeStartSource(ctx context.Context, tx pgx.Tx, fence run.ExecutionFence, computerID pgtype.UUID) error {
	secrets, err := run.LockSourceSecretsForComputer(ctx, tx, fence, computerID)
	if err != nil {
		return err
	}
	authority, _, err := secrets.LockLiveSource(ctx)
	if errors.Is(err, run.ErrExecutionTargetNotFound) {
		return ErrStartComputerNotFound
	}
	if err != nil {
		return err
	}
	if err = secrets.ValidateSourceDelivery(ctx); err != nil {
		return err
	}
	return CheckSourceSession(ctx, db.New(tx), authority.Session())
}

// CheckSourceSession admits a worker operation from a source Run's Session:
// a held Session is rejected with session_held, and then a Session whose
// active Turn has begun settlement with turn_unsettled. A source Run without
// a Session passes.
func CheckSourceSession(ctx context.Context, q db.Querier, source db.Session) error {
	if !source.ID.Valid {
		return nil
	}
	if source.DispatchHoldID.Valid {
		return &OperationError{Code: "session_held"}
	}
	if source.ActiveTurnID.Valid {
		turn, err := q.GetSessionTurn(ctx, db.GetSessionTurnParams{EnvironmentID: source.EnvironmentID, SessionID: source.ID, ID: source.ActiveTurnID})
		if err != nil {
			return err
		}
		if turn.SettlementStartedAt.Valid {
			return &OperationError{Code: "turn_unsettled"}
		}
	}
	return nil
}

func startedFromReceipt(raw []byte) (Started, error) {
	var receipt startReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return Started{}, ErrStartReceiptInvalid
	}
	sessionID, err := ids.Parse(receipt.SessionID)
	if err != nil {
		return Started{}, ErrStartReceiptInvalid
	}
	runID, err := ids.Parse(receipt.BootRunID)
	if err != nil {
		return Started{}, ErrStartReceiptInvalid
	}
	return Started{SessionID: sessionID, BootRunID: runID}, nil
}

func int8Ptr(value *int64) pgtype.Int8 {
	if value == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *value, Valid: true}
}

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
