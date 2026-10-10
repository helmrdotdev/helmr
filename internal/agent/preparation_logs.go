package agent

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// AppendPreparationLog accepts diagnostics from the attempt's sole live executor.
// Its identity cannot be supplied by a Session or reassigned to a successor.
func AppendPreparationLog(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, ref PreparationExecutor, record diagnostic.Record, bounds diagnostic.Bounds) (telemetry.DiagnosticReceipt, error) {
	if err := bounds.Validate(); err != nil {
		return telemetry.DiagnosticReceipt{}, err
	}
	if err := record.Validate(bounds.ChunkBytes); err != nil {
		return telemetry.DiagnosticReceipt{}, err
	}
	var result telemetry.DiagnosticReceipt
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		admission, err := telemetry.BeginDiagnosticAdmission(ctx, tx, telemetry.DiagnosticSource{EnvironmentID: ref.EnvironmentID, Kind: "computer_preparation", ID: ref.PreparationID, ProducerEpoch: ref.Epoch}, bounds)
		if err != nil {
			return err
		}
		if err := lockPreparationExecutor(ctx, tx, host, ref, true); err != nil {
			return err
		}
		result, err = admission.Append(ctx, record)
		if err != nil {
			return err
		}
		// Time spent admitting bytes cannot extend the executor's authority.
		return checkPreparationExecutor(ctx, tx, host, ref, true)
	})
	if err != nil {
		return telemetry.DiagnosticReceipt{}, hideMissing(err)
	}
	return result, nil
}
