//go:build linux && computerproof

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"
)

type nativeFinalizationDeadline struct {
	TurnID       string    `json:"turnId"`
	SessionID    string    `json:"sessionId"`
	SaveID       string    `json:"saveId"`
	QueuedTurnID string    `json:"queuedTurnId"`
	HoldID       string    `json:"holdId"`
	DeadlineAt   time.Time `json:"deadlineAt"`
	HeldAt       time.Time `json:"heldAt"`
}

func (g *nativePublicationGate) holdNativeDeadline(ctx context.Context, save string) error {
	var deadline bool
	err := g.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_saves s
 JOIN turns t ON(t.environment_id,t.id)=(s.environment_id,s.turn_id)
 JOIN sessions se ON(se.environment_id,se.id)=(t.environment_id,t.session_id)
 JOIN agents a ON(a.environment_id,a.id)=(se.environment_id,se.agent_id)
 WHERE s.environment_id=$1 AND s.id=$2 AND a.name='deadline')`, g.environment, save).Scan(&deadline)
	if err != nil || !deadline {
		return err
	}
	g.mu.Lock()
	observation := g.observed[save]
	if observation.DeadlineAttempted {
		g.mu.Unlock()
		return nil
	}
	observation.DeadlineAttempted = true
	observation.PublicationEnteredAt = time.Now().UTC()
	g.observed[save] = observation
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	proof := nativeFinalizationDeadline{SaveID: save}
	var valid bool
	err = g.pool.QueryRow(ctx, `SELECT t.id,t.session_id,t.deadline_at,clock_timestamp(),
 t.status='finalizing' AND t.seq=1 AND t.result IS NOT NULL AND t.completion_save_id IS NULL
 AND s.status='captured' AND s.root_id IS NULL AND s.captured_at<t.deadline_at
 AND t.deadline_at>=t.started_at+interval '30 seconds' AND t.deadline_at<=t.started_at+interval '31 seconds' AND clock_timestamp()<t.deadline_at
 FROM computer_saves s JOIN turns t ON(t.environment_id,t.id)=(s.environment_id,s.turn_id)
 WHERE s.environment_id=$1 AND s.id=$2`, g.environment, save).Scan(&proof.TurnID, &proof.SessionID, &proof.DeadlineAt, &proof.HeldAt, &valid)
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("deadline proof did not hold a captured Save before its configured elapsed deadline")
	}
	if err := writeNativeHandoffFile(filepath.Join(g.evidence, "deadline-held.json"), proof); err != nil {
		return err
	}
	if err := awaitLossCondition(ctx, func() (bool, error) {
		raw, err := os.ReadFile(filepath.Join(g.evidence, "deadline-release.json"))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		var released nativeFinalizationDeadline
		if err := json.Unmarshal(raw, &released); err != nil {
			return false, err
		}
		if released.TurnID != proof.TurnID || released.SessionID != proof.SessionID || released.SaveID != save || !released.DeadlineAt.Equal(proof.DeadlineAt) || !released.HeldAt.Equal(proof.HeldAt) || released.QueuedTurnID == "" || released.HoldID == "" {
			return false, errors.New("deadline release changed the held finalization")
		}
		proof = released
		err = verifyNativeDeadlineSave(ctx, g.pool, g.environment, proof, "captured", "queued")
		return err == nil, err
	}); err != nil {
		return err
	}
	g.mu.Lock()
	observation = g.observed[save]
	observation.Deadline = &proof
	observation.PublicationHoldMs = float64(time.Since(observation.PublicationEnteredAt)) / float64(time.Millisecond)
	g.observed[save] = observation
	g.mu.Unlock()
	return nil
}

func (g *nativePublicationGate) observeNativeDeadlinePublication(ctx context.Context, save string) error {
	g.mu.Lock()
	proof := g.observed[save].Deadline
	g.mu.Unlock()
	if proof == nil {
		return nil
	}
	if err := verifyNativeDeadlineSave(ctx, g.pool, g.environment, *proof, "published", "queued"); err != nil {
		return err
	}
	return writeNativeHandoffFile(filepath.Join(g.evidence, "deadline-published.json"), proof)
}

func verifyNativeDeadlineSave(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, proof nativeFinalizationDeadline, saveStatus, queuedStatus string) error {
	var valid bool
	err := pool.QueryRow(ctx, `SELECT
 t.status='interrupted' AND t.seq=1 AND t.result IS NOT NULL AND t.completion_save_id IS NULL
 AND t.result_recorded_at IS NOT NULL AND t.result_recorded_at<=s.captured_at
 AND s.captured_at<t.deadline_at AND t.deadline_at=$7 AND t.terminal_at>=t.deadline_at
 AND $10>=s.captured_at AND $10<t.deadline_at
 AND (SELECT count(DISTINCT (convert_from(e.data,'UTF8')::jsonb->0->>'text')::jsonb->>'count')>=2
      FROM session_events e JOIN turns peer ON(peer.environment_id,peer.id)=(e.environment_id,e.turn_id)
      JOIN sessions se ON(se.environment_id,se.id)=(peer.environment_id,peer.session_id)
      JOIN agents a ON(a.environment_id,a.id)=(se.environment_id,se.agent_id)
      WHERE e.environment_id=t.environment_id AND peer.computer_id=t.computer_id AND a.name='peer'
      AND e.kind='turn.output' AND e.created_at>=$10 AND e.created_at<t.deadline_at
      AND peer.started_at<=$10 AND (peer.terminal_at IS NULL OR peer.terminal_at>=t.deadline_at))
 AND t.deadline_at>=t.started_at+interval '30 seconds' AND t.deadline_at<=t.started_at+interval '31 seconds'
 AND s.status=$8 AND s.capture_evidence IS NOT NULL AND s.captured_root_digest IS NOT NULL
 AND (CASE WHEN $8='captured' THEN s.root_id IS NULL AND s.publication_evidence IS NULL
      ELSE s.publication_evidence IS NOT NULL AND (s.root_id IS NOT NULL OR s.payload_retired_at IS NOT NULL) END)
 AND queued.session_id=t.session_id AND queued.seq=2 AND queued.status=$9
 AND queued.started_at IS NULL AND queued.process_epoch IS NULL AND queued.completion_save_id IS NULL
 AND h.scope='local' AND h.issuer_kind='system' AND h.reason='Turn deadline elapsed' AND h.released_at IS NULL
 AND h.created_at>=t.deadline_at
 AND (SELECT count(*)=1 FROM session_holds other WHERE other.environment_id=t.environment_id AND other.session_id=t.session_id)
 AND EXISTS(SELECT 1 FROM session_events e WHERE e.environment_id=t.environment_id AND e.turn_id=t.id
      AND e.kind='session.deadline' AND convert_from(e.data,'UTF8')::jsonb->>'holdId'=h.id::text)
 AND NOT EXISTS(SELECT 1 FROM session_events e WHERE e.environment_id=t.environment_id AND e.turn_id=t.id AND e.kind='turn.completed')
 FROM turns t JOIN computer_saves s ON(s.environment_id,s.turn_id)=(t.environment_id,t.id)
 JOIN turns queued ON queued.environment_id=t.environment_id AND queued.id=$5
 JOIN session_holds h ON(h.environment_id,h.session_id)=(t.environment_id,t.session_id) AND h.id=$6
 WHERE t.environment_id=$1 AND t.session_id=$2 AND t.id=$3 AND s.id=$4`, env, proof.SessionID, proof.TurnID, proof.SaveID, proof.QueuedTurnID, proof.HoldID, proof.DeadlineAt, saveStatus, queuedStatus, proof.HeldAt).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("expired Turn or its local hold changed during %s Save publication", saveStatus)
	}
	return nil
}

func verifyNativeFinalizationDeadline(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, result []byte, evidence string) error {
	var receipt struct {
		Deadline *nativeFinalizationDeadline `json:"deadline"`
	}
	if err := json.Unmarshal(result, &receipt); err != nil {
		return err
	}
	if receipt.Deadline == nil || receipt.Deadline.DeadlineAt.IsZero() || receipt.Deadline.HeldAt.IsZero() {
		return errors.New("configured deadline evidence is missing")
	}
	// The SDK driver's final cleanup explicitly cancels retained queued input;
	// this cannot change the already-interrupted predecessor or release its hold.
	if err := verifyNativeDeadlineSave(ctx, pool, env, *receipt.Deadline, "published", "cancelled"); err != nil {
		return err
	}
	return writeNativeHandoffFile(filepath.Join(evidence, "finalization-deadline.json"), receipt.Deadline)
}
