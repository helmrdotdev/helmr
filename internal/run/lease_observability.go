package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// LogStore reads and appends a Run's log chunks outside a transaction.
type LogStore interface {
	GetRunLogChunkReplay(context.Context, db.GetRunLogChunkReplayParams) (db.GetRunLogChunkReplayRow, error)
	AppendRunLogChunk(context.Context, db.AppendRunLogChunkParams) (db.AppendRunLogChunkRow, error)
}

// LogChunk is one chunk a worker appends to a Run log stream under its lease
// receipt.
type LogChunk struct {
	// Fence addresses the lease; claim versions are not compared.
	Fence ExecutionFence
	// FenceFingerprint identifies the receipt the chunk was appended under.
	FenceFingerprint string
	Kind             string
	Severity         string
	Stream           string
	ObservedSeq      int64
	Payload          json.RawMessage
	Content          []byte
}

// AppendLog appends a log chunk without a transaction. A chunk already
// recorded at its stream sequence is a replay, accepted only when its
// content, payload and receipt match; a mismatch is ErrLogChunkDiffers. The
// append itself is fenced in its statement, and a receipt that addresses no
// live lease is pgx.ErrNoRows.
func AppendLog(ctx context.Context, store LogStore, chunk LogChunk) error {
	leaseID := chunk.Fence.LeaseID
	replay, err := store.GetRunLogChunkReplay(ctx, db.GetRunLogChunkReplayParams{
		RunLeaseID:  leaseID,
		Stream:      chunk.Stream,
		ObservedSeq: pgtype.Int8{Int64: chunk.ObservedSeq, Valid: true},
	})
	switch {
	case err == nil:
		payloadMatches, compareErr := equalJSON([]byte(replay.EventPayload), chunk.Payload)
		if compareErr != nil {
			return compareErr
		}
		if !bytes.Equal(replay.Content, chunk.Content) || !payloadMatches || replay.LeaseFenceFingerprint != chunk.FenceFingerprint {
			return ErrLogChunkDiffers
		}
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	row, err := store.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		Kind: chunk.Kind, Payload: chunk.Payload, Severity: chunk.Severity,
		LeaseFenceFingerprint: chunk.FenceFingerprint,
		RunLeaseID:            leaseID, LeaseSequence: chunk.Fence.LeaseSequence,
		WorkerGroupID: chunk.Fence.WorkerGroupID, WorkerHostID: chunk.Fence.WorkerHostID,
		WorkerEpoch: chunk.Fence.WorkerEpoch,
		Stream:      chunk.Stream, ObservedSeq: chunk.ObservedSeq, Content: chunk.Content,
	})
	if err != nil {
		return err
	}
	if !row.ReplayMatches {
		return ErrLogChunkDiffers
	}
	return nil
}

func equalJSON(left, right []byte) (bool, error) {
	leftCanonical, err := jsoncanon.Transform(left)
	if err != nil {
		return false, fmt.Errorf("canonicalize stored run log payload: %w", err)
	}
	rightCanonical, err := jsoncanon.Transform(right)
	if err != nil {
		return false, fmt.Errorf("canonicalize run log payload: %w", err)
	}
	return bytes.Equal(leftCanonical, rightCanonical), nil
}

// MetadataUpdate is a worker's idempotent mutation of its Run's metadata.
type MetadataUpdate struct {
	Fence       ExecutionFence
	OperationID uuid.UUID
	// Mutation is the canonical mutation the idempotency claim fingerprints.
	Mutation json.RawMessage
	// FenceFingerprint identifies the receipt the mutation was sent under.
	FenceFingerprint string
	// Apply returns the Run's next metadata from its current metadata.
	Apply func(current json.RawMessage) (json.RawMessage, error)
	// Event returns the validated payload of the metadata event the update
	// records.
	Event func() (json.RawMessage, error)
}

// UpdateMetadata applies a metadata mutation in its own transaction. It reads
// the lease's claim scope without locking, acquires the mutation's
// idempotency claim before any worker supply lock, and returns a completed
// replay there. A new mutation then locks the live execution, writes the
// metadata and its event and completes the claim with the receipt
// {"runId","revision"}. A receipt that no longer addresses the live
// execution is ErrStale; an expired or conflicting claim is the idempotency
// error; any other error is the mutation's rejection.
func UpdateMetadata(ctx context.Context, txb db.TxBeginner, update MetadataUpdate) error {
	fence := update.Fence
	return db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		scope, err := q.GetRunMetadataClaimScope(ctx, db.GetRunMetadataClaimScopeParams{
			RunLeaseID: fence.LeaseID, LeaseSequence: fence.LeaseSequence,
			WorkerGroupID: fence.WorkerGroupID, WorkerHostID: fence.WorkerHostID,
			WorkerEpoch: fence.WorkerEpoch,
		})
		if err != nil {
			return stale(err)
		}
		runID := pgvalue.MustUUIDValue(scope.RunID)
		claimRequest, err := idempotency.NewRunMetadataRequest(
			pgvalue.MustUUIDValue(scope.EnvironmentID),
			runID,
			scope.AttemptNumber,
			update.OperationID.String(),
			update.Mutation,
			update.FenceFingerprint,
		)
		if err != nil {
			return err
		}
		claims, err := idempotency.TransactionFor(tx)
		if err != nil {
			return err
		}
		acquired, err := claims.Acquire(ctx, claimRequest)
		if err != nil {
			return err
		}
		if !acquired.New {
			if acquired.Claim.Status != "completed" {
				return fmt.Errorf("run metadata mutation claim is %s", acquired.Claim.Status)
			}
			return nil
		}
		execution, err := LockLiveExecution(ctx, tx, fence)
		if err != nil {
			return stale(err)
		}
		r, attempt, lease := execution.run, execution.attempt, execution.lease
		if r.EnvironmentID != scope.EnvironmentID || r.ID != scope.RunID || attempt.Number != scope.AttemptNumber {
			return ErrStale
		}
		next, err := update.Apply(r.Metadata)
		if err != nil {
			return err
		}
		revision, err := q.UpdateRunMetadata(ctx, db.UpdateRunMetadataParams{
			Metadata: next, RunID: r.ID, AttemptNumber: attempt.Number, RunLeaseID: lease.ID,
		})
		if err != nil {
			return stale(err)
		}
		payload, err := update.Event()
		if err != nil {
			return err
		}
		if _, err := q.CreateRunMetadataEvent(ctx, db.CreateRunMetadataEventParams{
			OrgID: r.OrgID, RunID: r.ID,
			IdempotencyKey: pgvalue.Text("metadata:" + update.OperationID.String()),
			ProjectID:      r.ProjectID, EnvironmentID: r.EnvironmentID,
			RunLeaseID:    lease.ID,
			AttemptNumber: pgtype.Int4{Int32: attempt.Number, Valid: true},
			TraceID:       lease.TraceID, SpanID: lease.SpanID,
			ParentSpanID: lease.ParentSpanID, Traceparent: lease.Traceparent,
			Payload:         payload,
			SnapshotVersion: pgtype.Int8{Int64: revision, Valid: true},
		}); err != nil {
			return err
		}
		receipt, err := json.Marshal(map[string]any{"runId": runID.String(), "revision": revision})
		if err != nil {
			return err
		}
		_, err = claims.Complete(ctx, acquired.Claim, receipt)
		return err
	})
}
