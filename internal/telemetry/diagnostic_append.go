package telemetry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrDiagnosticBusy     = errors.New("diagnostic admission is busy")
	ErrDiagnosticInvalid  = diagnostic.ErrInvalid
	ErrDiagnosticCapacity = errors.New("diagnostic acceptance capacity unavailable")
	ErrDiagnosticConflict = errors.New("diagnostic retry changed its immutable record")
	ErrDiagnosticStale    = errors.New("diagnostic retry witness was superseded")
	ErrDiagnosticClosed   = errors.New("diagnostic stream is closed")
	ErrDiagnosticSequence = errors.New("diagnostic sequence does not follow accepted coverage")
)

// DiagnosticSource is a fixed owner identity, never a read or execution grant.
// Its owning operation authenticates the physical producer in the transaction.
type DiagnosticSource struct {
	EnvironmentID uuid.UUID
	Kind          string
	ID            uuid.UUID
	ProducerEpoch int64
}

func (s DiagnosticSource) Validate() error {
	if s.EnvironmentID == uuid.Nil() || s.ID == uuid.Nil() || s.ProducerEpoch <= 0 || (s.Kind != "session" && s.Kind != "computer_preparation" && s.Kind != "computer_command") {
		return ErrDiagnosticInvalid
	}
	return nil
}

// A disposition is effective only after the authorized transaction commits.
// Expired is a rejection of re-acceptance, not a new durability acknowledgment.
type DiagnosticReceipt struct {
	ThroughSequence int64
	AcceptedAt      time.Time
	ExpiresAt       time.Time
	Expired         bool
}

type diagnosticProgress struct {
	through        int64
	byteOffset     int64
	ended          bool
	expiredThrough int64
	lastSequence   int64
	digest         []byte
	acceptedAt     *time.Time
	expiresAt      *time.Time
}

// DiagnosticAdmission holds the transaction's queue gate and a fresh occupancy
// snapshot. Acquire it before lifecycle locks. Its short lock timeout ensures a
// busy producer cannot stall unrelated diagnostics behind this global gate.
type DiagnosticAdmission struct {
	tx                               pgx.Tx
	source                           DiagnosticSource
	bounds                           diagnostic.Bounds
	aggregate, environment, producer diagnosticUsage
}

func BeginDiagnosticAdmission(ctx context.Context, tx pgx.Tx, source DiagnosticSource, bounds diagnostic.Bounds) (DiagnosticAdmission, error) {
	a := DiagnosticAdmission{tx: tx, source: source, bounds: bounds}
	if tx == nil || source.Validate() != nil || bounds.Validate() != nil {
		return a, ErrDiagnosticInvalid
	}
	var acquired bool
	var isolation string
	if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation'),pg_try_advisory_xact_lock(1835363442,1684627815)`).Scan(&isolation, &acquired); err != nil {
		return a, err
	}
	if isolation != "read committed" {
		return a, ErrDiagnosticInvalid
	}
	if !acquired {
		return a, ErrDiagnosticBusy
	}
	// This statement is intentionally separate from lock acquisition: waiting for
	// another transaction must never leave admission using its preceding snapshot.
	for _, scope := range []struct {
		where string
		args  []any
		usage *diagnosticUsage
	}{
		{"", []any{bounds.QueueRecords + 1}, &a.aggregate},
		{" AND environment_id=$2", []any{bounds.EnvironmentRecords + 1, source.EnvironmentID}, &a.environment},
		{" AND environment_id=$2 AND source_kind=$3 AND source_id=$4 AND producer_epoch=$5", []any{bounds.SourceRecords + 1, source.EnvironmentID, source.Kind, source.ID, source.ProducerEpoch}, &a.producer},
	} {
		query := `SELECT COALESCE(sum(ingest_size_bytes),0)::bigint,count(*) FROM (SELECT ingest_size_bytes FROM telemetry_outbox WHERE stream_kind='diagnostic'` + scope.where + ` ORDER BY id LIMIT $1) pending`
		if err := tx.QueryRow(ctx, query, scope.args...).Scan(&scope.usage.bytes, &scope.usage.records); err != nil {
			return a, err
		}
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='25ms'`); err != nil {
		return a, err
	}
	return a, nil
}

// IsDiagnosticBusy distinguishes lock contention from a measured full queue.
func IsDiagnosticBusy(err error) bool {
	var p *pgconn.PgError
	return errors.Is(err, ErrDiagnosticBusy) || (errors.As(err, &p) && p.Code == "55P03")
}

func diagnosticOwner(source DiagnosticSource) (table, predicate string) {
	switch source.Kind {
	case "session":
		return "session_processes", "environment_id=$1 AND session_id=$2 AND epoch=$3"
	case "computer_preparation":
		return "computer_preparations", "environment_id=$1 AND id=$2 AND executor_epoch=$3"
	case "computer_command":
		return "computer_commands", "environment_id=$1 AND id=$2 AND $3::bigint=1"
	default:
		panic("validated diagnostic source required")
	}
}

// Append authenticates no producer itself: the owner must validate its exact
// writer after BeginDiagnosticAdmission and recheck timed authority before commit.
// One immutable head per pipe is replayable until its successor or original expiry.
func (a *DiagnosticAdmission) Append(ctx context.Context, record diagnostic.Record) (DiagnosticReceipt, error) {
	if a == nil || a.tx == nil || a.source.Validate() != nil || record.Validate(a.bounds.ChunkBytes) != nil {
		return DiagnosticReceipt{}, ErrDiagnosticInvalid
	}
	tx, source, bounds := a.tx, a.source, a.bounds
	table, predicate := diagnosticOwner(source)
	prefix := record.Stream + "_"
	args := []any{source.EnvironmentID, source.ID, source.ProducerEpoch}
	var progress diagnosticProgress
	query := fmt.Sprintf(`SELECT %[1]saccepted_through,%[1]sbyte_offset,%[1]sended,%[1]sexpired_through,COALESCE(%[1]slast_sequence,0),%[1]slast_digest,%[1]slast_accepted_at,%[1]slast_expires_at FROM %[2]s WHERE %[3]s FOR NO KEY UPDATE NOWAIT`, prefix, table, predicate)
	if err := tx.QueryRow(ctx, query, args...).Scan(&progress.through, &progress.byteOffset, &progress.ended, &progress.expiredThrough, &progress.lastSequence, &progress.digest, &progress.acceptedAt, &progress.expiresAt); err != nil {
		return DiagnosticReceipt{}, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return DiagnosticReceipt{}, err
	}
	if progress.expiresAt != nil && !progress.expiresAt.After(now) {
		progress.expiredThrough = progress.through
		query := fmt.Sprintf(`UPDATE %s SET %sexpired_through=%saccepted_through,%slast_digest=NULL WHERE %s`, table, prefix, prefix, prefix, predicate)
		if _, err := tx.Exec(ctx, query, args...); err != nil {
			return DiagnosticReceipt{}, err
		}
	}
	if record.Sequence <= progress.through {
		if record.ThroughSequence <= progress.expiredThrough {
			return DiagnosticReceipt{ThroughSequence: record.ThroughSequence, Expired: true}, nil
		}
		if record.Sequence != progress.lastSequence {
			return DiagnosticReceipt{}, ErrDiagnosticStale
		}
		digest := diagnosticDigest(source, record)
		if !bytes.Equal(progress.digest, digest[:]) {
			return DiagnosticReceipt{}, ErrDiagnosticConflict
		}
		if progress.acceptedAt == nil || progress.expiresAt == nil {
			return DiagnosticReceipt{}, ErrDiagnosticInvalid
		}
		return DiagnosticReceipt{ThroughSequence: progress.through, AcceptedAt: *progress.acceptedAt, ExpiresAt: *progress.expiresAt}, nil
	}
	if progress.ended {
		return DiagnosticReceipt{}, ErrDiagnosticClosed
	}
	if progress.through == math.MaxInt64 || record.Sequence != progress.through+1 {
		return DiagnosticReceipt{}, ErrDiagnosticSequence
	}
	size := int64(len(record.Data))
	if !a.aggregate.fits(size, bounds.QueueBytes, bounds.QueueRecords) || !a.environment.fits(size, bounds.EnvironmentBytes, bounds.EnvironmentRecords) || !a.producer.fits(size, bounds.SourceBytes, bounds.SourceRecords) {
		return DiagnosticReceipt{}, ErrDiagnosticCapacity
	}
	if record.DroppedBytes > math.MaxInt64-size || progress.byteOffset > math.MaxInt64-size-record.DroppedBytes {
		return DiagnosticReceipt{}, ErrDiagnosticInvalid
	}
	offset := progress.byteOffset + size + record.DroppedBytes
	if progress.acceptedAt != nil && progress.acceptedAt.After(now) {
		now = *progress.acceptedAt
	}
	receipt := DiagnosticReceipt{ThroughSequence: record.ThroughSequence, AcceptedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour)}
	data := record.Data
	if data == nil {
		data = []byte{}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO telemetry_outbox(environment_id,session_id,process_epoch,preparation_id,preparation_epoch,command_id,stream_kind,stream,sequence,through_sequence,byte_offset,through_byte_offset,kind,observed_at_unix_nano,data,dropped_bytes,complete,accepted_at,expires_at)
 VALUES($1,CASE WHEN $2='session' THEN $3::uuid END,CASE WHEN $2='session' THEN $4::bigint END,CASE WHEN $2='computer_preparation' THEN $3::uuid END,CASE WHEN $2='computer_preparation' THEN $4::bigint END,CASE WHEN $2='computer_command' THEN $3::uuid END,'diagnostic',$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, source.EnvironmentID, source.Kind, source.ID, source.ProducerEpoch, record.Stream, record.Sequence, record.ThroughSequence, progress.byteOffset, offset, record.Kind, record.ObservedAtUnixNano, data, record.DroppedBytes, record.Complete, receipt.AcceptedAt, receipt.ExpiresAt); err != nil {
		return DiagnosticReceipt{}, err
	}
	digest := diagnosticDigest(source, record)
	query = fmt.Sprintf(`UPDATE %[1]s SET %[2]saccepted_through=$4,%[2]sbyte_offset=$5,%[2]sended=$6,%[2]send_complete=$7,%[2]sgapped=%[2]sgapped OR $8,%[2]slast_sequence=$9,%[2]slast_digest=$10,%[2]slast_accepted_at=$11,%[2]slast_expires_at=$12,%[2]sexpired_through=$13 WHERE %[3]s`, table, prefix, predicate)
	if _, err := tx.Exec(ctx, query, source.EnvironmentID, source.ID, source.ProducerEpoch, record.ThroughSequence, offset, record.Kind == "end", record.Complete, record.Kind == "gap", record.Sequence, digest[:], receipt.AcceptedAt, receipt.ExpiresAt, progress.expiredThrough); err != nil {
		return DiagnosticReceipt{}, err
	}
	a.aggregate.bytes += size
	a.aggregate.records++
	a.environment.bytes += size
	a.environment.records++
	a.producer.bytes += size
	a.producer.records++
	return receipt, nil
}

// The canonical digest covers source identity and every immutable producer field.
// Database-assigned receipt times and replaceable physical authority are excluded.
func diagnosticDigest(source DiagnosticSource, record diagnostic.Record) [32]byte {
	h := sha256.New()
	h.Write([]byte("helmr-diagnostic-record-v1"))
	h.Write(source.EnvironmentID[:])
	h.Write(source.ID[:])
	for _, value := range []string{source.Kind, record.Stream, record.Kind} {
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(value))))
		h.Write([]byte(value))
	}
	for _, value := range []int64{source.ProducerEpoch, record.Sequence, record.ThroughSequence, record.ObservedAtUnixNano, record.DroppedBytes, int64(len(record.Data))} {
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(value)))
	}
	if record.Complete {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	h.Write(record.Data)
	return [32]byte(h.Sum(nil))
}

type diagnosticUsage struct{ bytes, records int64 }

func (u diagnosticUsage) fits(size, maxBytes, maxRecords int64) bool {
	return u.records < maxRecords && u.bytes <= maxBytes && size <= maxBytes-u.bytes
}
