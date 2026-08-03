package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	actordomain "github.com/helmrdotdev/helmr/internal/actor"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const maxActorInputBytes = 1 << 20
const maxActorSequence = int64(9007199254740991)

var (
	errActorInputAppendConflict = errors.New("actor input append conflicts with durable authority")
	errActorInputUnavailable    = errors.New("actor input append is unavailable")
	errActorInputTooLarge       = errors.New("actor input exceeds the maximum size")
	errActorSequenceExhausted   = errors.New("actor input sequence is exhausted")
)

type appendActorInputRequest struct {
	EnvironmentID  uuid.UUID
	ActorID        uuid.UUID
	RecordID       uuid.UUID
	Data           json.RawMessage
	SourceKind     string
	SourceRunID    uuid.UUID
	IdempotencyKey string
	Authorize      func(context.Context, db.Querier) error
}

// appendActorInput is the internal durable primitive behind ActorRef.input.send().
// Claim acquisition, append, receipt completion, wait wakeup, continuation
// admission, and repair intent are one transaction.
func (s *Server) appendActorInput(ctx context.Context, request appendActorInputRequest) (db.ActorRecord, error) {
	if request.EnvironmentID == uuid.Nil || request.ActorID == uuid.Nil || request.RecordID == uuid.Nil {
		return db.ActorRecord{}, errActorInputAppendConflict
	}
	if request.SourceKind != "external" && request.SourceKind != "run" {
		return db.ActorRecord{}, errActorInputAppendConflict
	}
	if (request.SourceKind == "run") != (request.SourceRunID != uuid.Nil) {
		return db.ActorRecord{}, errActorInputAppendConflict
	}
	if len(request.Data) == 0 || !json.Valid(request.Data) {
		return db.ActorRecord{}, errActorInputAppendConflict
	}
	canonical, err := canonicalJSON(request.Data)
	if err != nil {
		return db.ActorRecord{}, errActorInputAppendConflict
	}
	if len(canonical) > maxActorInputBytes {
		return db.ActorRecord{}, errActorInputTooLarge
	}
	var result db.ActorRecord
	err = s.inTx(ctx, func(work *txWork) error {
		claimID := pgtype.UUID{}
		fingerprint := []byte(nil)
		if request.IdempotencyKey != "" {
			claims, err := idempotency.TransactionForQueries(work.q)
			if err != nil {
				return err
			}
			idempotencyRequest, err := idempotency.NewActorInputSendRequest(
				request.EnvironmentID,
				request.ActorID,
				request.IdempotencyKey,
				canonical,
			)
			if err != nil {
				return errActorInputAppendConflict
			}
			acquired, err := claims.Acquire(ctx, idempotencyRequest)
			if err != nil {
				return err
			}
			if acquired.Claim.State == "completed" {
				replayed, err := actorInputRecordFromReceipt(request, acquired.Claim)
				if err != nil {
					return errActorInputAppendConflict
				}
				result = replayed
				return nil
			}
			if acquired.Claim.State != "pending" {
				return errActorInputAppendConflict
			}
			claimID = acquired.Claim.ID
			fingerprint = bytes.Clone(acquired.Claim.RequestFingerprint)
		}
		locator, err := work.q.GetActor(ctx, db.GetActorParams{
			EnvironmentID: pgvalue.UUID(request.EnvironmentID),
			ID:            pgvalue.UUID(request.ActorID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errActorInputUnavailable
		}
		if err != nil || !locator.WorkspaceID.Valid {
			return errActorInputAppendConflict
		}
		if locator.NextInputSequence > maxActorSequence {
			return errActorSequenceExhausted
		}
		bindings, err := work.q.LockWorkspaceSecretsForAdmission(ctx, locator.WorkspaceID)
		if err != nil {
			return fmt.Errorf("lock Actor input continuation Secret authority: %w", err)
		}
		sourceRunID := pgtype.UUID{}
		if request.SourceRunID != uuid.Nil {
			sourceRunID = pgvalue.UUID(request.SourceRunID)
		}
		appended, err := work.q.AppendActorInputRecord(ctx, db.AppendActorInputRecordParams{
			EnvironmentID: pgvalue.UUID(request.EnvironmentID), ClaimID: claimID,
			ActorID: pgvalue.UUID(request.ActorID), ExpectedRequestFingerprint: fingerprint,
			ID: pgvalue.UUID(request.RecordID), Data: canonical,
			SourceKind: pgvalue.Text(request.SourceKind), SourceRunID: sourceRunID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				current, readErr := work.q.GetActor(ctx, db.GetActorParams{
					EnvironmentID: pgvalue.UUID(request.EnvironmentID),
					ID:            pgvalue.UUID(request.ActorID),
				})
				if readErr == nil &&
					current.State == "open" &&
					current.NextInputSequence > maxActorSequence {
					return errActorSequenceExhausted
				}
				return errActorInputUnavailable
			}
			return err
		}
		if appended.ClaimFingerprintMismatch {
			return errActorInputAppendConflict
		}
		result = actorRecordFromAppend(appended)
		if request.Authorize != nil {
			if err := request.Authorize(ctx, work.q); err != nil {
				return err
			}
		}
		if claimID.Valid {
			_, claimErr := work.q.CompleteActorInputClaim(ctx, db.CompleteActorInputClaimParams{
				EnvironmentID: result.EnvironmentID, ClaimID: claimID,
				RequestFingerprint: fingerprint, ActorID: result.ActorID, RecordID: result.ID,
			})
			if errors.Is(claimErr, pgx.ErrNoRows) {
				claim, err := work.q.GetIdempotencyClaim(ctx, db.GetIdempotencyClaimParams{
					EnvironmentID: result.EnvironmentID, ID: claimID,
				})
				if err != nil || claim.State != "completed" || !bytes.Equal(claim.RequestFingerprint, fingerprint) {
					return errActorInputAppendConflict
				}
			} else if claimErr != nil {
				return fmt.Errorf("complete Actor input idempotency claim: %w", claimErr)
			}
		}

		actor, err := work.q.LockActorForInputReconcile(ctx, db.LockActorForInputReconcileParams{
			EnvironmentID: result.EnvironmentID, ActorID: result.ActorID,
		})
		if err != nil || actor.WorkspaceID != locator.WorkspaceID {
			return errActorInputAppendConflict
		}
		var currentRun db.Run
		if actor.CurrentRunID.Valid {
			// The Actor row remains locked for the transaction, so its current
			// Run cannot be replaced by completion while this read is used.
			// Avoiding a second Run lock also keeps A→B/B→A sends from acquiring
			// source and target Runs in opposite orders.
			currentRun, err = work.q.GetActorInputCurrentRun(ctx, db.GetActorInputCurrentRunParams{
				EnvironmentID: result.EnvironmentID, RunID: actor.CurrentRunID, ActorID: result.ActorID,
			})
			if err != nil {
				return errActorInputAppendConflict
			}
		}

		wait, waitErr := work.q.GetPendingActorInputRunWait(ctx, db.GetPendingActorInputRunWaitParams{
			EnvironmentID: result.EnvironmentID, ActorID: result.ActorID,
			RunID: currentRun.ID, AttemptNumber: currentRun.CurrentAttemptNumber,
			AfterInputSequence: pgtype.Int8{Int64: result.Sequence - 1, Valid: true},
		})
		if waitErr == nil {
			if _, err := actordomain.CompleteWait(ctx, work.q, wait, result); err != nil {
				return err
			}
		} else if !errors.Is(waitErr, pgx.ErrNoRows) {
			return waitErr
		}

		if actordomain.CanStartContinuation(actor) {
			workspace, err := work.q.LockActorInputWorkspace(ctx, db.LockActorInputWorkspaceParams{
				EnvironmentID: result.EnvironmentID, ID: actor.WorkspaceID, ActorID: result.ActorID,
			})
			if err != nil {
				return errActorInputAppendConflict
			}
			if _, err := actordomain.CreateContinuation(ctx, work.q, actor, workspace, bindings); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}

		return work.q.CreateActorInputReconcileOutbox(ctx, db.CreateActorInputReconcileOutboxParams{
			ID: result.ID, ActorID: result.ActorID,
			EnvironmentID: result.EnvironmentID, RecordID: result.ID,
		})
	})
	return result, err
}

type actorRecordClaimReceipt struct {
	RecordID string `json:"recordId"`
	Sequence int64  `json:"sequence"`
}

func actorInputRecordFromReceipt(request appendActorInputRequest, claim db.IdempotencyClaim) (db.ActorRecord, error) {
	var receipt actorRecordClaimReceipt
	if err := json.Unmarshal(claim.Receipt, &receipt); err != nil {
		return db.ActorRecord{}, err
	}
	recordID, err := ids.Parse(receipt.RecordID)
	if err != nil || recordID == uuid.Nil || receipt.Sequence <= 0 || receipt.Sequence > maxActorSequence {
		return db.ActorRecord{}, errActorInputAppendConflict
	}
	return db.ActorRecord{
		ID:            pgvalue.UUID(recordID),
		EnvironmentID: pgvalue.UUID(request.EnvironmentID),
		ActorID:       pgvalue.UUID(request.ActorID),
		Direction:     "input",
		Sequence:      receipt.Sequence,
		ClaimID:       claim.ID,
	}, nil
}

func actorRecordFromAppend(row db.AppendActorInputRecordRow) db.ActorRecord {
	return db.ActorRecord{
		ID: row.ID, EnvironmentID: row.EnvironmentID, ActorID: row.ActorID,
		Direction: row.Direction, Sequence: row.Sequence, Data: row.Data, ContentType: row.ContentType,
		SourceKind: row.SourceKind, SourceRunID: row.SourceRunID,
		ProducerRunID: row.ProducerRunID, ProducerAttemptNumber: row.ProducerAttemptNumber,
		ClaimID: row.ClaimID, CreatedAt: row.CreatedAt,
	}
}
