package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
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
	var request workerapi.UpdateRunMetadataRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker run metadata request JSON: %w", err))
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
	leaseFenceFingerprint, err := runLeaseFenceFingerprint(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	err = s.inTx(r.Context(), func(work *txWork) error {
		replayContext, err := work.q.GetRunMetadataClaimScope(
			r.Context(),
			runMetadataClaimScopeParams(request.Lease, parsed, worker),
		)
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		environmentID := pgvalue.MustUUIDValue(replayContext.EnvironmentID)
		runID := pgvalue.MustUUIDValue(replayContext.RunID)
		claimRequest, err := idempotency.NewRunMetadataRequest(
			environmentID,
			runID,
			replayContext.AttemptNumber,
			operationID.String(),
			mutation.canonical,
			leaseFenceFingerprint,
		)
		if err != nil {
			return err
		}
		claims, err := idempotency.TransactionFor(work.tx)
		if err != nil {
			return err
		}
		acquired, err := claims.Acquire(r.Context(), claimRequest)
		if err != nil {
			return err
		}
		if !acquired.New {
			if acquired.Claim.Status != "completed" {
				return fmt.Errorf(
					"run metadata mutation claim is %s",
					acquired.Claim.Status,
				)
			}
			return nil
		}
		authority, err := lockReceiptRunMutation(
			r.Context(),
			work.tx,
			worker,
			request.Lease,
			parsed,
		)
		if err != nil {
			return err
		}
		if authority.Run().EnvironmentID != replayContext.EnvironmentID ||
			authority.Run().ID != replayContext.RunID ||
			authority.Attempt().Number != replayContext.AttemptNumber {
			return errStaleRunLeaseClaim
		}
		next, err := applyRunMetadataMutation(authority.Run().Metadata, mutation)
		if err != nil {
			return err
		}
		next, err = normalizeMetadata(next, maxRunMetadataBytes, "run")
		if err != nil {
			return err
		}
		revision, err := work.q.UpdateRunMetadata(
			r.Context(),
			db.UpdateRunMetadataParams{
				Metadata: next, RunID: authority.Run().ID,
				AttemptNumber: authority.Attempt().Number,
				RunLeaseID:    authority.Lease().ID,
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
		if err := telemetry.ValidateEvent("Run metadata updated", payload); err != nil {
			return err
		}
		if _, err := work.q.CreateRunMetadataEvent(
			r.Context(),
			db.CreateRunMetadataEventParams{
				OrgID: authority.Run().OrgID, RunID: authority.Run().ID,
				IdempotencyKey: pgvalue.Text("metadata:" + operationID.String()),
				ProjectID:      authority.Run().ProjectID, EnvironmentID: authority.Run().EnvironmentID,
				RunLeaseID:    authority.Lease().ID,
				AttemptNumber: pgtype.Int4{Int32: authority.Attempt().Number, Valid: true},
				TraceID:       authority.Lease().TraceID, SpanID: authority.Lease().SpanID,
				ParentSpanID: authority.Lease().ParentSpanID, Traceparent: authority.Lease().Traceparent,
				Payload:         payload,
				SnapshotVersion: pgtype.Int8{Int64: revision, Valid: true},
			},
		); err != nil {
			return err
		}
		receipt, err := json.Marshal(map[string]any{
			"runId": runID.String(), "revision": revision,
		})
		if err != nil {
			return err
		}
		_, err = claims.Complete(r.Context(), acquired.Claim, receipt)
		return err
	})
	if err != nil {
		var expired idempotency.ExpiredError
		if errors.As(err, &expired) {
			writeError(w, gone(expired))
			return
		}
		var conflictErr idempotency.ConflictError
		if writeStaleWorkerClaims(w, err) {
			return
		}
		switch {
		case errors.As(err, &conflictErr):
			writeError(w, conflict(conflictErr))
		case errors.Is(err, errStaleRunLeaseClaim), errors.Is(err, pgx.ErrNoRows):
			writeError(w, conflict(errors.New("worker run lease fence is stale")))
		default:
			s.log.Error("update Run metadata failed", "run_lease_id", request.Lease.ID, "error", err)
			writeError(w, apiError{kind: errUnprocessable, err: codedError{
				code: "run_metadata_rejected", message: err.Error(),
			}})
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerAppendStructuredLog(w http.ResponseWriter, r *http.Request) {
	var request workerapi.StructuredLogRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker structured log request JSON: %w", err))
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
	content, err := canonicalJSON(mustJSON(map[string]any{
		"level": request.Level, "message": request.Message,
		"attributes": json.RawMessage(attributes),
	}))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := telemetry.ValidateRunLog(content); err != nil {
		writeError(w, badRequest(err))
		return
	}
	parsed, worker, err := s.parseWorkerRunMutation(r, request.Lease)
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
	row, err := s.appendRunLog(
		r.Context(),
		worker,
		request.Lease,
		parsed,
		db.AppendRunLogChunkParams{
			Kind: "log.structured", Payload: payload, Severity: request.Level,
			Stream:      string(workerapi.LogStreamStructured),
			ObservedSeq: int64(request.ObservedSeq), Content: content,
		},
	)
	if isNoRows(err) || errors.Is(err, errStaleRunLeaseClaim) {
		writeError(w, conflict(errors.New(
			"worker run lease is stale or the structured log sequence contains different content",
		)))
		return
	}
	if err != nil {
		s.log.Error("append structured Run log failed", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("append structured run log"))
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
	lease workerapi.RunLeaseFence,
) (parsedRunLeaseFence, workergroup.HostPrincipal, error) {
	parsed, err := parseRunLeaseFence(lease)
	if err != nil {
		return parsedRunLeaseFence{}, workergroup.HostPrincipal{}, badRequest(err)
	}
	worker := workerFromContext(r.Context())
	return parsed, worker, nil
}

func lockReceiptRunMutation(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, lease workerapi.RunLeaseFence, parsed parsedRunLeaseFence) (run.Execution, error) {
	authority, err := run.LockLiveExecution(ctx, tx, run.ExecutionFence{LeaseID: pgvalue.UUID(parsed.leaseID), LeaseSequence: lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.GroupID), WorkerHostID: pgvalue.UUID(worker.HostID), WorkerEpoch: worker.Epoch, GroupClaimVersion: worker.GroupClaimVersion, HostClaimVersion: worker.HostClaimVersion})
	if err != nil {
		return run.Execution{}, staleRunLeaseClaim(err)
	}
	return authority, nil
}

func normalizeRunMetadataMutation(
	request workerapi.UpdateRunMetadataRequest,
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
		value, err := canonicalJSON(request.Value)
		if err != nil {
			return runMetadataMutation{}, fmt.Errorf("set value is invalid: %w", err)
		}
		mutation.key = request.Key
		mutation.value = value
	case "patch":
		if request.Key != "" || len(request.Value) != 0 || request.Amount != nil {
			return runMetadataMutation{}, errors.New("patch requires only patch")
		}
		patch, err := normalizeMetadata(request.Patch, maxRunMetadataBytes, "run metadata patch")
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
	canonical, err := canonicalJSON(mustJSON(map[string]any{
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
			return nil, fmt.Errorf("stored run metadata is invalid: %w", err)
		}
	}
	switch mutation.operation {
	case "set":
		values[mutation.key] = mutation.value
	case "patch":
		maps.Copy(values, mutation.patch)
	case "increment":
		currentValue := float64(0)
		if raw, ok := values[mutation.key]; ok {
			if err := json.Unmarshal(raw, &currentValue); err != nil ||
				math.IsNaN(currentValue) || math.IsInf(currentValue, 0) {
				return nil, fmt.Errorf(
					"run metadata key %q is not a finite number",
					mutation.key,
				)
			}
		}
		next := currentValue + *mutation.amount
		if math.IsNaN(next) || math.IsInf(next, 0) {
			return nil, fmt.Errorf(
				"run metadata increment for key %q is not finite",
				mutation.key,
			)
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return nil, err
		}
		values[mutation.key] = raw
	default:
		return nil, errors.New("run metadata mutation is invalid")
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
