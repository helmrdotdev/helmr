package agent

import (
	"context"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// The caller holds the captured members' ownership locks. Durable receipts
// prove the current controls without relying on host-reported generations.
func requireComputerControlReceipts(ctx context.Context, tx pgx.Tx, env uuid.UUID, r captureRecord) error {
	for _, m := range r.Members {
		var stopped, acknowledged bool
		var kind *string
		if err := tx.QueryRow(ctx, `SELECT p.status='stopped' AND p.fenced_at IS NOT NULL,
 p.attachment_sequence>0 AND p.control_attachment=p.attachment_sequence AND p.control_generation=s.authority_generation
 AND p.control_sequence>0 AND p.control_acknowledged_at IS NOT NULL AND p.control_error IS NULL,p.control_kind
 FROM sessions s JOIN session_processes p ON (p.environment_id,p.session_id)=(s.environment_id,s.id)
 WHERE s.environment_id=$1 AND s.id=$2 AND p.epoch=$3`, env, m.Session, m.Epoch).Scan(&stopped, &acknowledged, &kind); err != nil {
			return err
		}
		desired, err := desiredSessionControl(ctx, tx, Execution{EnvironmentID: env, SessionID: m.Session, ProcessEpoch: m.Epoch})
		if err != nil {
			return err
		}
		if desired == "shutdown" {
			// A delivered shutdown is not proof of physical termination. A stopped
			// process needs no further transport receipt and cannot be resurrected.
			if !stopped {
				return ErrNotReady
			}
		} else if !acknowledged || kind == nil || *kind != desired {
			return ErrNotReady
		}
	}
	return nil
}
