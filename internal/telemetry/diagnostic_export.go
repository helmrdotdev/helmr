package telemetry

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"github.com/jackc/pgx/v5"
)

// StoredDiagnostic keeps the original acceptance clock through
// every export retry. Physical sender identity is deliberately not part of it.
type StoredDiagnostic struct {
	ByteOffset, ThroughByteOffset int64
	Source                        DiagnosticSource
	Record                        diagnostic.Record
	AcceptedAt                    time.Time
	ExpiresAt                     time.Time
}

var ErrDiagnosticExportSize = errors.New("diagnostic export byte bound is smaller than a pending record")

type DiagnosticExportClaim struct {
	Token   uuid.UUID
	IDs     []int64
	Records []StoredDiagnostic
}

// ClaimDiagnostics holds no database lock while the sink is called. A new claim
// token fences a slow exporter whose claim expired before its sink reply arrived.
func ClaimDiagnostics(ctx context.Context, pool db.TxBeginner, kind string, bounds diagnostic.ExportBounds) (DiagnosticExportClaim, error) {
	if kind != "session" && kind != "computer_preparation" && kind != "computer_command" {
		return DiagnosticExportClaim{}, ErrDiagnosticInvalid
	}
	if err := bounds.Validate(); err != nil {
		return DiagnosticExportClaim{}, err
	}
	result := DiagnosticExportClaim{Token: uuid.NewV7()}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		candidates, err := tx.Query(ctx, `SELECT o.id,octet_length(o.data)::bigint
 FROM telemetry_outbox o
 WHERE o.stream_kind='diagnostic' AND o.source_kind=$1 AND o.export_after<=statement_timestamp() AND o.expires_at>statement_timestamp()
 ORDER BY o.export_after,o.id LIMIT $2 FOR UPDATE OF o SKIP LOCKED`, kind, bounds.Records)
		if err != nil {
			return err
		}
		var ids []int64
		remaining := bounds.Bytes
		full := false
		for candidates.Next() {
			var id, size int64
			if err = candidates.Scan(&id, &size); err != nil {
				candidates.Close()
				return err
			}
			if size > bounds.Bytes {
				candidates.Close()
				return ErrDiagnosticExportSize
			}
			if size > remaining {
				full = true
			}
			if !full {
				ids = append(ids, id)
				remaining -= size
			}
		}
		err = candidates.Err()
		candidates.Close()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		rows, err := tx.Query(ctx, `WITH claimed AS (
 UPDATE telemetry_outbox SET export_claim=$2,export_after=statement_timestamp()+$3::bigint*interval '1 microsecond'
 WHERE id=ANY($1) RETURNING *
 ) SELECT id,environment_id,source_kind,source_id,producer_epoch,stream,kind,sequence,through_sequence,observed_at_unix_nano,data,dropped_bytes,complete,byte_offset,through_byte_offset,accepted_at,expires_at FROM claimed ORDER BY id`, ids, result.Token, bounds.ClaimFor.Microseconds())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var r StoredDiagnostic
			if err = rows.Scan(&id, &r.Source.EnvironmentID, &r.Source.Kind, &r.Source.ID, &r.Source.ProducerEpoch, &r.Record.Stream, &r.Record.Kind, &r.Record.Sequence, &r.Record.ThroughSequence, &r.Record.ObservedAtUnixNano, &r.Record.Data, &r.Record.DroppedBytes, &r.Record.Complete, &r.ByteOffset, &r.ThroughByteOffset, &r.AcceptedAt, &r.ExpiresAt); err != nil {
				return err
			}
			result.IDs = append(result.IDs, id)
			result.Records = append(result.Records, r)
		}
		return rows.Err()
	})
	if err != nil {
		return DiagnosticExportClaim{}, err
	}
	return result, nil
}

// RetryDiagnostics retains accepted bytes and all capacity charges. It does not
// store sink error text, which may contain sensitive authored output.
func RetryDiagnostics(ctx context.Context, pool db.TxBeginner, token uuid.UUID, ids []int64, after time.Duration) error {
	if token == uuid.Nil() || after < time.Microsecond {
		return ErrDiagnosticInvalid
	}
	if len(ids) == 0 {
		return nil
	}
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE telemetry_outbox SET export_after=statement_timestamp()+$3::bigint*interval '1 microsecond',export_claim=NULL WHERE id=ANY($1) AND export_claim=$2`, ids, token, after.Microseconds())
		return err
	})
}

type DiagnosticRetirement struct{ Delivered, Expired int64 }

// RetireDiagnostics is called only after durable sink acceptance. Expiry during
// sink I/O is counted as expired coverage, never as successful historical delivery.
// The token prevents an older exporter from retiring a newer claimant's rows.
func RetireDiagnostics(ctx context.Context, pool db.TxBeginner, token uuid.UUID, ids []int64) (DiagnosticRetirement, error) {
	if token == uuid.Nil() {
		return DiagnosticRetirement{}, ErrDiagnosticInvalid
	}
	if len(ids) == 0 {
		return DiagnosticRetirement{}, nil
	}
	return retireDiagnostics(ctx, pool, token, ids, 0)
}

// ExpireDiagnostics retires expired pending bytes even during a sink outage.
// A separate bounded pass clears expired latest digests with no pending payload.
func ExpireDiagnostics(ctx context.Context, pool db.TxBeginner, limit int) (DiagnosticRetirement, error) {
	if limit <= 0 {
		return DiagnosticRetirement{}, ErrDiagnosticInvalid
	}
	return retireDiagnostics(ctx, pool, uuid.Nil(), nil, limit)
}

func retireDiagnostics(ctx context.Context, pool db.TxBeginner, token uuid.UUID, ids []int64, limit int) (DiagnosticRetirement, error) {
	var result DiagnosticRetirement
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		// No owner locks or accounting writes. Deleting pending payload can only make
		// a concurrent admission scan conservative; all increases hold the gate.
		if token == uuid.Nil() {
			return tx.QueryRow(ctx, `WITH candidates AS (
    SELECT id FROM telemetry_outbox WHERE stream_kind='diagnostic' AND expires_at<=statement_timestamp()
    ORDER BY expires_at,id LIMIT $1 FOR UPDATE SKIP LOCKED
   ), removed AS (DELETE FROM telemetry_outbox o USING candidates c WHERE o.id=c.id AND o.expires_at<=statement_timestamp() RETURNING o.id)
   SELECT 0::bigint,count(*) FROM removed`, limit).Scan(&result.Delivered, &result.Expired)
		}
		return tx.QueryRow(ctx, `WITH removed AS (
   DELETE FROM telemetry_outbox WHERE stream_kind='diagnostic' AND id=ANY($1) AND export_claim=$2
   RETURNING expires_at<=statement_timestamp() AS expired
  ) SELECT count(*) FILTER(WHERE NOT expired),count(*) FILTER(WHERE expired) FROM removed`, ids, token).Scan(&result.Delivered, &result.Expired)
	})
	return result, err
}
