package agent

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// AppendSessionLog admits diagnostics from the current physical attachment of an
// exact logical process. A physically stopped process may deliver its retained
// tail while that same Computer lease still owns custody. This grants no business
// operation, process restart or inference about the active Turn.
func AppendSessionLog(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, attachment int64, record diagnostic.Record, bounds diagnostic.Bounds) (telemetry.DiagnosticReceipt, error) {
	if attachment <= 0 {
		return telemetry.DiagnosticReceipt{}, ErrInvalidInput
	}
	if err := bounds.Validate(); err != nil {
		return telemetry.DiagnosticReceipt{}, err
	}
	if err := record.Validate(bounds.ChunkBytes); err != nil {
		return telemetry.DiagnosticReceipt{}, err
	}
	var result telemetry.DiagnosticReceipt
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		admission, err := telemetry.BeginDiagnosticAdmission(ctx, tx, telemetry.DiagnosticSource{EnvironmentID: e.EnvironmentID, Kind: "session", ID: e.SessionID, ProducerEpoch: e.ProcessEpoch}, bounds)
		if err != nil {
			return err
		}
		if _, err := lockRuntimeProcessAuthority(ctx, tx, host, e, true); err != nil {
			return err
		}
		var current int64
		if err := tx.QueryRow(ctx, `SELECT attachment_sequence FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&current); err != nil {
			return err
		}
		if current != attachment {
			return ErrDenied
		}
		result, err = admission.Append(ctx, record)
		if err != nil {
			return err
		}
		// Acceptance cannot extend time-limited physical authority. All earlier
		// locks remain held, so a failed recheck rolls back bytes and capacity together.
		_, err = lockRuntimeProcessAuthority(ctx, tx, host, e, true)
		return err
	})
	if err != nil {
		return telemetry.DiagnosticReceipt{}, hideMissing(err)
	}
	return result, nil
}
