package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/actorinput"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const maxActorInputBytes = 1 << 20

var (
	errActorInputAppendConflict = errors.New("Actor input append conflicts with durable authority")
	errActorInputUnavailable    = errors.New("Actor input append is unavailable")
)

type appendActorInputRequest struct {
	EnvironmentID      uuid.UUID
	ActorID            uuid.UUID
	RecordID           uuid.UUID
	Data               json.RawMessage
	SourceKind         string
	SourceRunID        uuid.UUID
	ClaimID            uuid.UUID
	RequestFingerprint []byte
}

// appendActorInput is the internal durable primitive behind ActorRef.input.send().
// It intentionally has no public HTTP/SDK transport until the Actor management
// surface is implemented, but its transaction is complete and independently testable.
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
	if (request.ClaimID != uuid.Nil) != (len(request.RequestFingerprint) == 32) {
		return db.ActorRecord{}, errActorInputAppendConflict
	}
	if len(request.Data) == 0 || len(request.Data) > maxActorInputBytes || !json.Valid(request.Data) {
		return db.ActorRecord{}, errActorInputAppendConflict
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, request.Data); err != nil {
		return db.ActorRecord{}, errActorInputAppendConflict
	}
	locator, err := s.db.GetActor(ctx, db.GetActorParams{
		EnvironmentID: pgvalue.UUID(request.EnvironmentID), ID: pgvalue.UUID(request.ActorID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.ActorRecord{}, errActorInputUnavailable
	}
	if err != nil || !locator.WorkspaceID.Valid {
		return db.ActorRecord{}, errActorInputAppendConflict
	}

	var result db.ActorRecord
	err = s.inTx(ctx, func(work *txWork) error {
		claimID := pgtype.UUID{}
		fingerprint := []byte(nil)
		if request.ClaimID != uuid.Nil {
			claimID = pgvalue.UUID(request.ClaimID)
			fingerprint = request.RequestFingerprint
			claim, err := work.q.LockActorInputClaim(ctx, db.LockActorInputClaimParams{
				EnvironmentID: pgvalue.UUID(request.EnvironmentID), ID: claimID,
			})
			if err != nil || (claim.State != "pending" && claim.State != "completed") ||
				!bytes.Equal(claim.RequestFingerprint, fingerprint) {
				return errActorInputAppendConflict
			}
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
			ID: pgvalue.UUID(request.RecordID), Data: compact.Bytes(),
			SourceKind: pgvalue.Text(request.SourceKind), SourceRunID: sourceRunID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errActorInputUnavailable
			}
			return err
		}
		if appended.ClaimFingerprintMismatch {
			return errActorInputAppendConflict
		}
		result = actorRecordFromAppend(appended)
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
			currentRun, err = work.q.LockActorInputCurrentRun(ctx, db.LockActorInputCurrentRunParams{
				EnvironmentID: result.EnvironmentID, RunID: actor.CurrentRunID, ActorID: result.ActorID,
			})
			if err != nil {
				return errActorInputAppendConflict
			}
			workspace, err := work.q.LockActorInputWorkspace(ctx, db.LockActorInputWorkspaceParams{
				EnvironmentID: result.EnvironmentID, ID: actor.WorkspaceID, ActorID: result.ActorID,
			})
			if err != nil {
				return errActorInputAppendConflict
			}
			attempt, err := work.q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
				RunID: currentRun.ID, Number: currentRun.CurrentAttemptNumber, WorkspaceID: workspace.ID,
			})
			if err != nil || attempt.TerminalAt.Valid {
				return errActorInputAppendConflict
			}
		}

		wait, waitErr := work.q.GetPendingActorInputRunWait(ctx, db.GetPendingActorInputRunWaitParams{
			EnvironmentID: result.EnvironmentID, ActorID: result.ActorID,
			RunID: currentRun.ID, AttemptNumber: currentRun.CurrentAttemptNumber,
			AfterInputSequence: pgtype.Int8{Int64: result.Sequence - 1, Valid: true},
		})
		if waitErr == nil {
			if _, err := actorinput.CompleteWait(ctx, work.q, wait, result); err != nil {
				return err
			}
		} else if !errors.Is(waitErr, pgx.ErrNoRows) {
			return waitErr
		}

		if actorinput.CanStartContinuation(actor) {
			workspace, err := work.q.LockActorInputWorkspace(ctx, db.LockActorInputWorkspaceParams{
				EnvironmentID: result.EnvironmentID, ID: actor.WorkspaceID, ActorID: result.ActorID,
			})
			if err != nil {
				return errActorInputAppendConflict
			}
			if _, err := actorinput.CreateContinuation(ctx, work.q, actor, workspace, bindings); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}

		reconcileRecordID, err := pgvalue.UUIDValue(result.ID)
		if err != nil {
			return errActorInputAppendConflict
		}
		reconcileID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("helmr.actor-input-reconcile.v1:"+reconcileRecordID.String()))
		return work.q.CreateActorInputReconcileOutbox(ctx, db.CreateActorInputReconcileOutboxParams{
			ID: pgvalue.UUID(reconcileID), ActorID: result.ActorID,
			EnvironmentID: result.EnvironmentID, RecordID: result.ID,
		})
	})
	return result, err
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
