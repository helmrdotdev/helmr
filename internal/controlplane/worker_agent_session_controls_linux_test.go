//go:build linux && computerproof

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"
)

func verifyNativeSessionControls(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, result []byte, evidence string) error {
	var receipt struct {
		PeerSessionID string `json:"peerSessionId"`
		Discovery     []struct {
			Owned    string `json:"owned"`
			Controls struct {
				ExactInterruptedTurn string `json:"exactInterruptedTurn"`
				ExactFollowingTurn   string `json:"exactFollowingTurn"`
				InterruptedTurn      string `json:"interruptedTurn"`
				ResumedTurn          string `json:"resumedTurn"`
				HoldID               string `json:"holdId"`
				CancelledSession     string `json:"cancelledSession"`
				CancelledTurn        string `json:"cancelledTurn"`
				CancelledQueuedTurn  string `json:"cancelledQueuedTurn"`
			} `json:"controls"`
		} `json:"discovery"`
	}
	if err := json.Unmarshal(result, &receipt); err != nil {
		return err
	}
	if len(receipt.Discovery) != 3 {
		return errors.New("missing native Session control observations")
	}
	d := receipt.Discovery[0]
	c := d.Controls
	var valid bool
	var observed json.RawMessage
	err := pool.QueryRow(ctx, `SELECT COALESCE((
 interrupted.status='interrupted' AND interrupted.result IS NULL AND interrupted.completion_save_id IS NULL
 AND resumed.status='completed' AND resumed.completion_save_id IS NOT NULL
 AND resumed.process_epoch=interrupted.process_epoch
 AND exact.status='interrupted' AND exact.result IS NULL AND exact.completion_save_id IS NULL
 AND following.status='completed' AND following.completion_save_id IS NOT NULL
 AND following.process_epoch=exact.process_epoch AND exact.process_epoch=interrupted.process_epoch
 AND following.started_at>=exact.terminal_at
 AND h.scope='subtree' AND h.issuer_kind='session' AND h.issuer_id=$3
 AND h.released_at>=h.created_at+interval '20 seconds'
 AND resumed.started_at>=h.released_at
 AND p.status='stopped' AND p.fenced_at>=resumed.terminal_at AND p.stop_turn_id IS NULL
 AND stopped.status='cancelled' AND stopped.started_at IS NOT NULL
 AND queued.status='cancelled' AND queued.started_at IS NULL AND queued.process_epoch IS NULL
 AND cp.status='stopped' AND cp.fenced_at IS NOT NULL),false),
 jsonb_build_object('interruptedTurn',interrupted.id,'resumedTurn',resumed.id,'holdId',h.id,
  'holdCreatedAt',h.created_at,'holdReleasedAt',h.released_at,'oldProcessFencedAt',p.fenced_at,
  'retainedProcessEpoch',p.epoch,'exactInterruptedTurn',exact.id,'exactFollowingTurn',following.id,
  'successorStartedAt',resumed.started_at,'cancelledTurn',stopped.id,'cancelledQueuedTurn',queued.id,
  'cancelledProcessFencedAt',cp.fenced_at)
 FROM turns interrupted
 JOIN turns exact ON exact.environment_id=interrupted.environment_id AND exact.session_id=interrupted.session_id AND exact.id=$10
 JOIN turns following ON following.environment_id=exact.environment_id AND following.session_id=exact.session_id AND following.id=$11
 JOIN turns resumed ON resumed.environment_id=interrupted.environment_id AND resumed.session_id=interrupted.session_id
 JOIN session_holds h ON h.environment_id=interrupted.environment_id AND h.session_id=interrupted.session_id
 JOIN session_processes p ON (p.environment_id,p.session_id,p.epoch)=(interrupted.environment_id,interrupted.session_id,interrupted.process_epoch)
 JOIN turns stopped ON stopped.environment_id=interrupted.environment_id AND stopped.session_id=$7 AND stopped.id=$8
 JOIN turns queued ON queued.environment_id=stopped.environment_id AND queued.session_id=stopped.session_id AND queued.id=$9
 JOIN session_processes cp ON (cp.environment_id,cp.session_id,cp.epoch)=(stopped.environment_id,stopped.session_id,stopped.process_epoch)
 WHERE interrupted.environment_id=$1 AND interrupted.session_id=$2 AND interrupted.id=$4 AND resumed.id=$5 AND h.id=$6`,
		env, d.Owned, receipt.PeerSessionID, c.InterruptedTurn, c.ResumedTurn, c.HoldID, c.CancelledSession, c.CancelledTurn, c.CancelledQueuedTurn, c.ExactInterruptedTurn, c.ExactFollowingTurn).Scan(&valid, &observed)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(evidence, "session-controls.json"), observed, 0600); err != nil {
		return err
	}
	if !valid {
		return errors.New("native Session control authority, ordering, or physical stop evidence failed")
	}
	return nil
}
