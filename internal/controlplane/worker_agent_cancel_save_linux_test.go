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

type nativeFinalizationCancellation struct {
	TurnID       string `json:"turnId"`
	SessionID    string `json:"sessionId"`
	SaveID       string `json:"saveId"`
	QueuedTurnID string `json:"queuedTurnId"`
}

// Keep the real publication request pending while the SDK cancels its Session.
// Only fixture handshakes are written here; all Turn/Save changes use ordinary APIs.
func (g *nativePublicationGate) holdNativeCancellation(ctx context.Context, save string) error {
	g.mu.Lock()
	observation, native := g.observed[save]
	if !native || observation.TurnSequence != 3 || observation.CancellationAttempted {
		g.mu.Unlock()
		return nil
	}
	// A failed proof remains a test error, but must not trap ordinary Save
	// reconciliation or other Sessions behind a repeatedly injected hold.
	observation.CancellationAttempted = true
	g.observed[save] = observation
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	proof := nativeFinalizationCancellation{SaveID: save}
	var valid bool
	err := g.pool.QueryRow(ctx, `SELECT t.id,t.session_id,
 t.status='finalizing' AND t.result IS NOT NULL AND t.completion_save_id IS NULL
 AND s.status='captured' AND s.root_id IS NULL AND t.seq=3
 FROM computer_saves s JOIN turns t ON(t.environment_id,t.id)=(s.environment_id,s.turn_id)
 WHERE s.environment_id=$1 AND s.id=$2`, g.environment, save).Scan(&proof.TurnID, &proof.SessionID, &valid)
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("cancellation proof did not begin at captured unpublished finalization")
	}
	if err := writeNativeHandoffFile(filepath.Join(g.evidence, "cancel-held-"+proof.TurnID+".json"), proof); err != nil {
		return err
	}
	if err := awaitLossCondition(ctx, func() (bool, error) {
		raw, err := os.ReadFile(filepath.Join(g.evidence, "cancel-release-"+proof.TurnID+".json"))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		var released nativeFinalizationCancellation
		if err := json.Unmarshal(raw, &released); err != nil {
			return false, err
		}
		if released.TurnID != proof.TurnID || released.SessionID != proof.SessionID || released.SaveID != save || released.QueuedTurnID == "" {
			return false, errors.New("cancellation release changed the held finalization")
		}
		proof.QueuedTurnID = released.QueuedTurnID
		err = verifyNativeCancelledSave(ctx, g.pool, g.environment, proof, "captured")
		return err == nil, err
	}); err != nil {
		return err
	}
	g.mu.Lock()
	observation = g.observed[save]
	observation.Cancellation = &proof
	g.observed[save] = observation
	g.mu.Unlock()
	return nil
}

func (g *nativePublicationGate) observeNativeCancelledPublication(ctx context.Context, save string) error {
	g.mu.Lock()
	proof := g.observed[save].Cancellation
	g.mu.Unlock()
	if proof == nil {
		return nil
	}
	if err := verifyNativeCancelledSave(ctx, g.pool, g.environment, *proof, "published"); err != nil {
		return err
	}
	return writeNativeHandoffFile(filepath.Join(g.evidence, "cancel-published-"+proof.TurnID+".json"), proof)
}

func verifyNativeCancelledSave(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, proof nativeFinalizationCancellation, saveStatus string) error {
	var valid bool
	err := pool.QueryRow(ctx, `SELECT
 t.status='cancelled' AND t.result IS NOT NULL AND t.completion_save_id IS NULL AND t.seq=3
 AND t.processing_closed_at IS NOT NULL AND t.result_recorded_at IS NOT NULL
 AND t.terminal_at>=s.captured_at AND s.status=$6
 AND s.capture_evidence IS NOT NULL AND s.captured_root_digest IS NOT NULL
 AND (CASE WHEN $6='captured' THEN s.root_id IS NULL AND s.publication_evidence IS NULL
      ELSE s.publication_evidence IS NOT NULL AND (s.root_id IS NOT NULL OR s.payload_retired_at IS NOT NULL) END)
 AND queued.session_id=t.session_id AND queued.seq=4 AND queued.status='cancelled'
 AND queued.started_at IS NULL AND queued.process_epoch IS NULL AND queued.completion_save_id IS NULL
 AND (SELECT count(*)=2 AND bool_and(prior.status='completed' AND prior.completion_save_id IS NOT NULL)
      FROM turns prior WHERE prior.environment_id=t.environment_id AND prior.session_id=t.session_id AND prior.seq<3)
 AND NOT EXISTS(SELECT 1 FROM session_events e WHERE e.environment_id=t.environment_id AND e.turn_id=t.id AND e.kind='turn.completed')
 FROM turns t JOIN computer_saves s ON(s.environment_id,s.turn_id)=(t.environment_id,t.id)
 JOIN turns queued ON queued.environment_id=t.environment_id AND queued.id=$5
 WHERE t.environment_id=$1 AND t.session_id=$2 AND t.id=$3 AND s.id=$4`, env, proof.SessionID, proof.TurnID, proof.SaveID, proof.QueuedTurnID, saveStatus).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("cancelled Turn or its queued successor changed during %s Save publication", saveStatus)
	}
	return nil
}

func verifyNativeFinalizationCancellation(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, result []byte, evidence string) error {
	var receipt struct {
		NativeSessions map[string]string `json:"nativeSessions"`
		Cancellations  []struct {
			nativeFinalizationCancellation
			Provider string `json:"provider"`
		} `json:"cancellations"`
	}
	if err := json.Unmarshal(result, &receipt); err != nil {
		return err
	}
	if len(receipt.Cancellations) != 2 {
		return errors.New("both native cancellation receipts are required")
	}
	seen := map[string]bool{}
	for _, proof := range receipt.Cancellations {
		if (proof.Provider != "codex" && proof.Provider != "claude") || seen[proof.Provider] || proof.SessionID != receipt.NativeSessions[proof.Provider] {
			return errors.New("native cancellation provider or Session binding changed")
		}
		seen[proof.Provider] = true
		if err := verifyNativeCancelledSave(ctx, pool, env, proof.nativeFinalizationCancellation, "published"); err != nil {
			return err
		}
	}
	return writeNativeHandoffFile(filepath.Join(evidence, "finalization-cancellations.json"), receipt.Cancellations)
}
