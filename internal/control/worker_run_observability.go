package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	maxRunMetadataKeyBytes      = 512
	maxStructuredMessageBytes   = 4 << 10
	maxStructuredAttributeBytes = 16 << 10
)

type runMetadataMutation struct {
	operation string
	key       string
	value     json.RawMessage
	patch     map[string]json.RawMessage
	amount    *float64
	canonical json.RawMessage
}

func (s *Server) workerUpdateRunMetadata(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerUpdateRunMetadataRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker Run metadata request JSON: %w", err)))
		return
	}
	operationID, err := parseCanonicalUUID("operation_id", request.OperationID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	mutation, err := normalizeRunMetadataMutation(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	parsed, worker, err := s.parseWorkerRunMutation(r, request.Lease)
	if err != nil {
		writeError(w, err)
		return
	}
	err = s.inTx(r.Context(), func(work *txWork) error {
		authority, err := lockReceiptRunMutation(
			r.Context(),
			work.q,
			worker,
			request.Lease,
			parsed,
		)
		if err != nil {
			return err
		}
		environmentID := pgvalue.MustUUIDValue(authority.run.EnvironmentID)
		runID := pgvalue.MustUUIDValue(authority.run.ID)
		claimRequest, err := idempotency.NewRunMetadataRequest(
			environmentID,
			runID,
			authority.attempt.Number,
			operationID.String(),
			mutation.canonical,
		)
		if err != nil {
			return err
		}
		claims, err := s.claims.TransactionForQueries(work.q)
		if err != nil {
			return err
		}
		acquired, err := claims.Acquire(r.Context(), claimRequest)
		if err != nil {
			return err
		}
		if !acquired.New {
			if acquired.Claim.State != "completed" {
				return fmt.Errorf(
					"Run metadata mutation claim is %s",
					acquired.Claim.State,
				)
			}
			return nil
		}
		next, err := applyRunMetadataMutation(authority.run.Metadata, mutation)
		if err != nil {
			return err
		}
		next, err = normalizeMetadata(next, maxRunMetadataBytes, "Run")
		if err != nil {
			return err
		}
		stateVersion, err := work.q.UpdateRunMetadata(
			r.Context(),
			db.UpdateRunMetadataParams{
				Metadata: next, RunID: authority.run.ID,
				AttemptNumber: authority.attempt.Number,
				RunLeaseID:    authority.runLease.ID,
			},
		)
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		payload, err := json.Marshal(map[string]any{
			"operation":    mutation.operation,
			"operation_id": operationID.String(),
			"key":          mutation.key,
		})
		if err != nil {
			return err
		}
		if _, err := work.q.CreateRunMetadataEvent(
			r.Context(),
			db.CreateRunMetadataEventParams{
				OrgID: authority.run.OrgID, RunID: authority.run.ID,
				IdempotencyKey: pgvalue.Text("metadata:" + operationID.String()),
				ProjectID:      authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
				RunLeaseID:    authority.runLease.ID,
				AttemptNumber: pgtype.Int4{Int32: authority.attempt.Number, Valid: true},
				TraceID:       authority.runLease.TraceID, SpanID: authority.runLease.SpanID,
				ParentSpanID: authority.runLease.ParentSpanID, Traceparent: authority.runLease.Traceparent,
				Payload:         payload,
				SnapshotVersion: pgtype.Int8{Int64: stateVersion, Valid: true},
			},
		); err != nil {
			return err
		}
		receipt, err := json.Marshal(map[string]any{
			"runId": runID.String(), "stateVersion": stateVersion,
		})
		if err != nil {
			return err
		}
		_, err = claims.Complete(r.Context(), acquired.Claim, receipt)
		return err
	})
	if err != nil {
		var conflictErr idempotency.ConflictError
		switch {
		case errors.As(err, &conflictErr):
			writeError(w, conflict(conflictErr))
		case errors.Is(err, errStaleRunLeaseClaim), errors.Is(err, pgx.ErrNoRows):
			writeError(w, conflict(errors.New("worker Run Lease receipt is stale")))
		default:
			s.log.Error("update Run metadata failed", "run_id", request.Lease.RunID, "error", err)
			writeError(w, apiError{kind: errUnprocessable, err: codedError{
				code: "run_metadata_rejected", message: err.Error(),
			}})
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerAppendStructuredLog(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerStructuredLogRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker structured log request JSON: %w", err)))
		return
	}
	if request.ObservedSeq > uint64(math.MaxInt64) {
		writeError(w, badRequest(errors.New("observed_seq is too large")))
		return
	}
	switch request.Level {
	case "debug", "info", "warn", "error":
	default:
		writeError(w, badRequest(errors.New("level must be debug, info, warn, or error")))
		return
	}
	if !utf8.ValidString(request.Message) ||
		len([]byte(request.Message)) > maxStructuredMessageBytes {
		writeError(w, badRequest(fmt.Errorf(
			"message must be valid UTF-8 no larger than %d bytes",
			maxStructuredMessageBytes,
		)))
		return
	}
	attributes, err := normalizeMetadata(
		request.Attributes,
		maxStructuredAttributeBytes,
		"structured log attributes",
	)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	content, err := jsoncanon.Transform(mustJSON(map[string]any{
		"level": request.Level, "message": request.Message,
		"attributes": json.RawMessage(attributes),
	}))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	parsed, _, err := s.parseWorkerRunMutation(r, request.Lease)
	if err != nil {
		writeError(w, err)
		return
	}
	payload, err := json.Marshal(map[string]any{
		"level": request.Level, "message": request.Message,
		"attributes":   json.RawMessage(attributes),
		"observed_seq": request.ObservedSeq,
	})
	if err != nil {
		writeError(w, errors.New("encode structured log"))
		return
	}
	row, err := s.appendReceiptRunLog(
		r.Context(),
		request.Lease,
		parsed,
		db.AppendReceiptRunLogChunkParams{
			Kind: "log.structured", Payload: payload, Severity: request.Level,
			Stream:      string(api.WorkerLogStreamStructured),
			ObservedSeq: int64(request.ObservedSeq), Content: content,
		},
	)
	if isNoRows(err) {
		writeError(w, conflict(errors.New(
			"worker Run Lease is stale or the structured log sequence contains different content",
		)))
		return
	}
	if err != nil {
		s.log.Error("append structured Run log failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("append structured Run log"))
		return
	}
	if !row.ReplayMatches {
		writeError(w, conflict(errors.New(
			"structured log sequence already contains different content",
		)))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) parseWorkerRunMutation(
	r *http.Request,
	lease api.WorkerRunLeaseReceipt,
) (parsedRunLeaseReceipt, workerActor, error) {
	parsed, err := parseRunLeaseReceipt(lease)
	if err != nil {
		return parsedRunLeaseReceipt{}, workerActor{}, badRequest(err)
	}
	worker := workerFromContext(r.Context())
	if lease.WorkerGroupID != worker.WorkerGroupID ||
		parsed.workerInstanceID != worker.WorkerInstanceID ||
		lease.WorkerEpoch != worker.WorkerEpoch ||
		lease.WorkerProtocolVersion != worker.ProtocolVersion {
		return parsedRunLeaseReceipt{}, workerActor{}, forbidden(
			errors.New("worker Run Lease receipt belongs to another worker epoch"),
		)
	}
	return parsed, worker, nil
}

func lockReceiptRunMutation(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	lease api.WorkerRunLeaseReceipt,
	parsed parsedRunLeaseReceipt,
) (runLeaseClaimAuthority, error) {
	locators, err := q.GetLiveRunLeaseLocators(
		ctx,
		db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: lease.LeaseSequence,
			WorkerGroupID:    worker.WorkerGroupID,
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:      worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
		},
	)
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	authority, err := lockLiveRunLeaseAuthority(
		ctx,
		q,
		worker,
		pgvalue.UUID(parsed.leaseID),
		lease.LeaseSequence,
		locators,
	)
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}
	current, err := projectRunLeaseReceipt(runLeaseProjectionAuthority{
		run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
		networkSlot: authority.networkSlot, runLease: authority.runLease,
		workspace: authority.workspace, workspaceMount: authority.workspaceMount,
		workspaceLease: authority.workspaceLease,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}
	if !equalRunLeaseReceipt(current, lease) {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	return authority, nil
}

func normalizeRunMetadataMutation(
	request api.WorkerUpdateRunMetadataRequest,
) (runMetadataMutation, error) {
	mutation := runMetadataMutation{operation: request.Operation}
	switch request.Operation {
	case "set":
		if err := validateMetadataKey(request.Key); err != nil {
			return runMetadataMutation{}, err
		}
		if len(request.Value) == 0 || len(request.Patch) != 0 || request.Amount != nil {
			return runMetadataMutation{}, errors.New("set requires only key and value")
		}
		value, err := jsoncanon.Transform(request.Value)
		if err != nil {
			return runMetadataMutation{}, fmt.Errorf("set value is invalid: %w", err)
		}
		mutation.key = request.Key
		mutation.value = value
	case "patch":
		if request.Key != "" || len(request.Value) != 0 || request.Amount != nil {
			return runMetadataMutation{}, errors.New("patch requires only patch")
		}
		patch, err := normalizeMetadata(request.Patch, maxRunMetadataBytes, "Run metadata patch")
		if err != nil {
			return runMetadataMutation{}, err
		}
		if err := json.Unmarshal(patch, &mutation.patch); err != nil {
			return runMetadataMutation{}, err
		}
		for key := range mutation.patch {
			if err := validateMetadataKey(key); err != nil {
				return runMetadataMutation{}, err
			}
		}
	case "increment":
		if err := validateMetadataKey(request.Key); err != nil {
			return runMetadataMutation{}, err
		}
		if len(request.Value) != 0 || len(request.Patch) != 0 ||
			request.Amount == nil || math.IsNaN(*request.Amount) ||
			math.IsInf(*request.Amount, 0) {
			return runMetadataMutation{}, errors.New("increment requires only key and a finite amount")
		}
		mutation.key = request.Key
		amount := *request.Amount
		mutation.amount = &amount
	default:
		return runMetadataMutation{}, errors.New("operation must be set, patch, or increment")
	}
	canonical, err := jsoncanon.Transform(mustJSON(map[string]any{
		"operation": mutation.operation, "key": mutation.key,
		"value": mutation.value, "patch": mutation.patch, "amount": mutation.amount,
	}))
	if err != nil {
		return runMetadataMutation{}, err
	}
	mutation.canonical = canonical
	return mutation, nil
}

func applyRunMetadataMutation(
	current json.RawMessage,
	mutation runMetadataMutation,
) (json.RawMessage, error) {
	values := make(map[string]json.RawMessage)
	if len(current) != 0 {
		if err := json.Unmarshal(current, &values); err != nil {
			return nil, fmt.Errorf("stored Run metadata is invalid: %w", err)
		}
	}
	switch mutation.operation {
	case "set":
		values[mutation.key] = mutation.value
	case "patch":
		for key, value := range mutation.patch {
			values[key] = value
		}
	case "increment":
		currentValue := float64(0)
		if raw, ok := values[mutation.key]; ok {
			if err := json.Unmarshal(raw, &currentValue); err != nil ||
				math.IsNaN(currentValue) || math.IsInf(currentValue, 0) {
				return nil, fmt.Errorf(
					"Run metadata key %q is not a finite number",
					mutation.key,
				)
			}
		}
		next := currentValue + *mutation.amount
		if math.IsNaN(next) || math.IsInf(next, 0) {
			return nil, fmt.Errorf(
				"Run metadata increment for key %q is not finite",
				mutation.key,
			)
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return nil, err
		}
		values[mutation.key] = raw
	default:
		return nil, errors.New("Run metadata mutation is invalid")
	}
	return json.Marshal(values)
}

func validateMetadataKey(value string) error {
	if value == "" || !utf8.ValidString(value) ||
		len([]byte(value)) > maxRunMetadataKeyBytes {
		return fmt.Errorf(
			"metadata key must be nonempty UTF-8 no larger than %d bytes",
			maxRunMetadataKeyBytes,
		)
	}
	return nil
}

func mustJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}
