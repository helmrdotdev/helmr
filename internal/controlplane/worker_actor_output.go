package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const maxActorOutputBytes = 1 << 20
const maxActorOutputContentTypeBytes = 255

var (
	errStaleActorOutputAppend    = errors.New("actor output append source authority is stale")
	errActorOutputAppendConflict = errors.New("actor output append conflicts with durable authority")
	errActorOutputTooLarge       = errors.New("actor output exceeds the maximum size")
	errActorOutputUnavailable    = errors.New("actor output append is unavailable")
)

type parsedWorkerActorOutputAppend struct {
	lease          parsedRunLeaseFence
	correlationID  uuid.UUID
	data           json.RawMessage
	contentType    string
	idempotencyKey string
}

func (s *Server) workerAppendActorOutput(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.AppendActorOutputRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid actor output append JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid actor output append JSON: trailing value")))
		return
	}
	parsed, err := parseWorkerActorOutputAppend(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())

	record, err := s.appendActorOutput(r.Context(), worker, request, parsed)
	if err != nil {
		if failure, ok := actorOutputAppendFailure(err); ok {
			writeJSON(w, http.StatusOK, workerapi.AppendActorOutputResponse{
				CorrelationID: request.CorrelationID,
				Failed:        &failure,
			})
			return
		}
		if errors.Is(err, errStaleActorOutputAppend) {
			writeError(w, conflict(errStaleActorOutputAppend))
			return
		}
		s.log.Error("append Actor output", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("append actor output"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AppendActorOutputResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &record,
	})
}

func parseWorkerActorOutputAppend(
	request workerapi.AppendActorOutputRequest,
) (parsedWorkerActorOutputAppend, error) {
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return parsedWorkerActorOutputAppend{}, err
	}
	correlationID, err := parseCanonicalUUID("correlation_id", request.CorrelationID)
	if err != nil {
		return parsedWorkerActorOutputAppend{}, err
	}
	canonical, err := canonicalJSON(request.Data)
	if err != nil {
		return parsedWorkerActorOutputAppend{}, errors.New("data must be valid JSON")
	}
	contentType := strings.TrimSpace(request.ContentType)
	if contentType == "" || len(contentType) > maxActorOutputContentTypeBytes {
		return parsedWorkerActorOutputAppend{}, fmt.Errorf(
			"content_type must be between 1 and %d bytes",
			maxActorOutputContentTypeBytes,
		)
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		return parsedWorkerActorOutputAppend{}, err
	}
	return parsedWorkerActorOutputAppend{
		lease:          lease,
		correlationID:  correlationID,
		data:           canonical,
		contentType:    contentType,
		idempotencyKey: idempotencyKey,
	}, nil
}

func (s *Server) appendActorOutput(
	ctx context.Context,
	worker workerActor,
	request workerapi.AppendActorOutputRequest,
	parsed parsedWorkerActorOutputAppend,
) (api.SessionOutput, error) {
	if len(parsed.data) > maxActorOutputBytes {
		return api.SessionOutput{}, errActorOutputTooLarge
	}
	locatorParams := db.GetLiveRunLeaseLocatorsParams{
		ID:                    pgvalue.UUID(parsed.lease.leaseID),
		LeaseSequence:         request.Lease.LeaseSequence,
		WorkerGroupID:         worker.WorkerGroupID,
		WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:           worker.WorkerEpoch,
		WorkerProtocolVersion: worker.ProtocolVersion,
	}
	discovered, err := s.db.GetLiveRunLeaseLocators(ctx, locatorParams)
	if err != nil || !discovered.SessionID.Valid {
		return api.SessionOutput{}, staleActorOutputAppend(err)
	}
	environmentID, err := pgvalue.UUIDValue(discovered.EnvironmentID)
	if err != nil {
		return api.SessionOutput{}, errStaleActorOutputAppend
	}
	actorID, err := pgvalue.UUIDValue(discovered.SessionID)
	if err != nil {
		return api.SessionOutput{}, errStaleActorOutputAppend
	}
	var response api.SessionOutput
	err = s.inTx(ctx, func(work *txWork) error {
		claimID := pgtype.UUID{}
		fingerprint := []byte(nil)
		var acquiredClaim db.IdempotencyClaim
		if parsed.idempotencyKey != "" {
			claims, err := idempotency.TransactionForQueries(work.q)
			if err != nil {
				return err
			}
			claimRequest, err := idempotency.NewActorOutputAppendRequest(
				environmentID,
				actorID,
				parsed.idempotencyKey,
				parsed.data,
				parsed.contentType,
			)
			if err != nil {
				return errActorOutputAppendConflict
			}
			acquired, err := claims.Acquire(ctx, claimRequest)
			if err != nil {
				return err
			}
			if acquired.Claim.State != "pending" && acquired.Claim.State != "completed" {
				return errActorOutputAppendConflict
			}
			acquiredClaim = acquired.Claim
			claimID = acquired.Claim.ID
			fingerprint = bytes.Clone(acquired.Claim.RequestFingerprint)
		}
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, locatorParams)
		if err != nil ||
			locators.EnvironmentID != discovered.EnvironmentID ||
			locators.SessionID != discovered.SessionID {
			return staleActorOutputAppend(err)
		}
		if _, err := secret.LockAttemptDelivery(
			ctx, work.q, locators.RunID, locators.AttemptNumber, locators.WorkspaceID,
		); err != nil {
			return fmt.Errorf("lock actor output secret authority: %w", err)
		}
		owner, err := lockRunFinalizationOwner(ctx, work.q, locators)
		if err != nil || !owner.actor.ID.Valid {
			return staleActorOutputAppend(err)
		}
		authority, err := lockLiveRunLeaseAuthority(
			ctx,
			work.q,
			worker,
			pgvalue.UUID(parsed.lease.leaseID),
			request.Lease.LeaseSequence,
			locators,
		)
		if err != nil {
			return staleActorOutputAppend(err)
		}
		authority.actor = owner.actor
		if authority.run.ParentRunID.Valid ||
			authority.run.EntrypointKind != "actor" ||
			authority.run.SessionID != authority.actor.ID ||
			authority.actor.CurrentRunID != authority.run.ID ||
			(authority.actor.State != "open" && authority.actor.State != "closing") ||
			authority.run.Status != db.RunStatusRunning ||
			authority.runLease.State != db.RunLeaseStateRunning ||
			!authority.run.ActiveStartedAt.Valid ||
			!authority.attempt.EntrypointEnteredAt.Valid ||
			authority.attempt.TerminalAt.Valid ||
			authority.runLease.FinalizationOperationID.Valid {
			return errStaleActorOutputAppend
		}
		if acquiredClaim.State == "completed" {
			record, err := actorOutputRecordFromReceipt(ctx, work.q, authority, acquiredClaim)
			if err != nil {
				return errActorOutputAppendConflict
			}
			response, err = projectAppendedActorOutput(ctx, work.q, record)
			return err
		}
		if authority.actor.NextOutputSequence > maxSessionOutputSequence {
			return errActorSequenceExhausted
		}

		row, err := work.q.AppendActorOutputRecord(ctx, db.AppendActorOutputRecordParams{
			EnvironmentID:              authority.run.EnvironmentID,
			ClaimID:                    claimID,
			SessionID:                  authority.actor.ID,
			ProducerRunID:              authority.run.ID,
			ProducerAttemptNumber:      authority.attempt.Number,
			ExpectedRequestFingerprint: fingerprint,
			ID:                         pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Data:                       parsed.data,
			ContentType:                parsed.contentType,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errActorOutputUnavailable
		}
		if err != nil {
			return err
		}
		if row.ClaimFingerprintMismatch {
			return errActorOutputAppendConflict
		}
		record := actorOutputRecordFromAppend(row)
		if claimID.Valid {
			if _, err := work.q.CompleteActorOutputClaim(ctx, db.CompleteActorOutputClaimParams{
				EnvironmentID:      record.EnvironmentID,
				ClaimID:            claimID,
				RequestFingerprint: fingerprint,
				SessionID:          record.SessionID,
				RecordID:           record.ID,
			}); err != nil {
				return fmt.Errorf("complete actor output idempotency claim: %w", err)
			}
		}
		response, err = projectAppendedActorOutput(ctx, work.q, record)
		return err
	})
	return response, err
}

func actorOutputRecordFromReceipt(
	ctx context.Context,
	q db.Querier,
	authority runLeaseClaimAuthority,
	claim db.IdempotencyClaim,
) (db.SessionRecord, error) {
	var receipt actorRecordClaimReceipt
	if err := json.Unmarshal(claim.Receipt, &receipt); err != nil {
		return db.SessionRecord{}, err
	}
	recordID, err := ids.Parse(receipt.SessionRecordID)
	if err != nil || recordID == uuid.Nil || receipt.Sequence <= 0 || receipt.Sequence > maxSessionOutputSequence {
		return db.SessionRecord{}, errActorOutputAppendConflict
	}
	record, err := q.GetActorOutputRecordByID(ctx, db.GetActorOutputRecordByIDParams{
		EnvironmentID: authority.run.EnvironmentID,
		SessionID:     authority.actor.ID,
		ID:            pgvalue.UUID(recordID),
	})
	if err != nil ||
		record.Sequence != receipt.Sequence ||
		!record.ProducerRunID.Valid ||
		!record.ProducerAttemptNumber.Valid {
		return db.SessionRecord{}, errActorOutputAppendConflict
	}
	return record, nil
}

func actorOutputRecordFromAppend(row db.AppendActorOutputRecordRow) db.SessionRecord {
	return db.SessionRecord{
		ID: row.ID, EnvironmentID: row.EnvironmentID, SessionID: row.SessionID,
		Direction: row.Direction, Sequence: row.Sequence, Data: row.Data, ContentType: row.ContentType,
		SourceKind: row.SourceKind, SourceRunID: row.SourceRunID,
		ProducerRunID: row.ProducerRunID, ProducerAttemptNumber: row.ProducerAttemptNumber,
		ClaimID: row.ClaimID, CreatedAt: row.CreatedAt,
	}
}

func projectAppendedActorOutput(
	ctx context.Context,
	q db.Querier,
	record db.SessionRecord,
) (api.SessionOutput, error) {
	recordUUID, err := pgvalue.UUIDValue(record.ID)
	if err != nil {
		return api.SessionOutput{}, errActorOutputAppendConflict
	}
	if ids.Validate(recordUUID.String()) != nil {
		return api.SessionOutput{}, errActorOutputAppendConflict
	}
	if record.Direction != "output" ||
		!record.ProducerAttemptNumber.Valid ||
		record.ProducerAttemptNumber.Int32 < 1 ||
		record.Sequence <= 0 ||
		record.Sequence > maxSessionOutputSequence ||
		!json.Valid(record.Data) ||
		record.ContentType == "" ||
		!record.CreatedAt.Valid {
		return api.SessionOutput{}, errActorOutputAppendConflict
	}
	run, err := q.GetRun(ctx, db.GetRunParams{
		EnvironmentID: record.EnvironmentID,
		ID:            record.ProducerRunID,
	})
	if err != nil || run.SessionID != record.SessionID {
		return api.SessionOutput{}, errActorOutputAppendConflict
	}
	runID := pgvalue.UUIDString(run.ID)
	deploymentID := pgvalue.UUIDString(run.DeploymentID)
	if ids.Validate(runID) != nil {
		return api.SessionOutput{}, errActorOutputAppendConflict
	}
	if ids.Validate(deploymentID) != nil {
		return api.SessionOutput{}, errActorOutputAppendConflict
	}
	return api.SessionOutput{
		ID: recordUUID.String(), Sequence: record.Sequence, Data: append(json.RawMessage(nil), record.Data...),
		ContentType: record.ContentType, CreatedAt: record.CreatedAt.Time.UTC(),
		Provenance: api.SessionOutputProvenance{
			RunID: runID, AttemptNumber: record.ProducerAttemptNumber.Int32,
			DeploymentID: deploymentID,
		},
	}, nil
}

func staleActorOutputAppend(err error) error {
	if err == nil {
		return errStaleActorOutputAppend
	}
	return errors.Join(errStaleActorOutputAppend, err)
}

func actorOutputAppendFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	var conflictError idempotency.ConflictError
	switch {
	case errors.As(err, &conflictError):
		return workerapi.RuntimeOperationFailure{
			Code: "idempotency_conflict", Message: "idempotency key conflicts with an earlier Actor output",
		}, true
	case errors.Is(err, errActorOutputTooLarge):
		return workerapi.RuntimeOperationFailure{Code: "actor_output_too_large", Message: err.Error()}, true
	case errors.Is(err, errActorSequenceExhausted):
		return workerapi.RuntimeOperationFailure{Code: "actor_sequence_exhausted", Message: err.Error()}, true
	case errors.Is(err, errActorOutputUnavailable):
		return workerapi.RuntimeOperationFailure{Code: "actor_not_open", Message: "Actor does not accept output"}, true
	case errors.Is(err, errActorOutputAppendConflict):
		return workerapi.RuntimeOperationFailure{Code: "actor_output_conflict", Message: err.Error()}, true
	default:
		return workerapi.RuntimeOperationFailure{}, false
	}
}
