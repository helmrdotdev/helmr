//go:build linux && computerproof

package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"
)

func writeNativeStopObservations(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, path string) error {
	var raw []byte
	err := pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(observed),'[]'::jsonb) FROM (
 SELECT jsonb_build_object('sessionId',session_id,'processEpoch',epoch,'status',status,
  'stopSequence',stop_sequence,'stopTurnId',stop_turn_id,'stopDeadlineAt',stop_deadline_at,
  'fencedAt',fenced_at,'observedAt',clock_timestamp()) observed
 FROM session_processes WHERE environment_id=$1 ORDER BY session_id,epoch LIMIT 100) p`, env).Scan(&raw)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".new", raw, 0600); err != nil {
		return err
	}
	return os.Rename(path+".new", path)
}

func verifyNativeStops(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, result []byte, evidence string) error {
	var receipt struct {
		NativeSessions map[string]string `json:"nativeSessions"`
		NativeStops    []struct {
			Provider          string `json:"provider"`
			SessionID         string `json:"sessionId"`
			InterruptedTurnID string `json:"interruptedTurnId"`
			SuccessorTurnID   string `json:"successorTurnId"`
			AskID             string `json:"askId"`
			Samples           []struct {
				ProcessEpoch int64     `json:"processEpoch"`
				StopSequence int64     `json:"stopSequence"`
				ObservedAt   time.Time `json:"observedAt"`
			} `json:"samples"`
		} `json:"nativeStops"`
	}
	if err := json.Unmarshal(result, &receipt); err != nil {
		return err
	}
	if len(receipt.NativeStops) != 4 {
		return fmt.Errorf("native stop count: %d", len(receipt.NativeStops))
	}
	counts := map[string]int{}
	var observations []json.RawMessage
	for _, stop := range receipt.NativeStops {
		if (stop.Provider != "codex" && stop.Provider != "claude") || stop.SessionID != receipt.NativeSessions[stop.Provider] || len(stop.Samples) != 3 {
			return fmt.Errorf("invalid native stop identity or pending samples")
		}
		counts[stop.Provider]++
		first, last := stop.Samples[0], stop.Samples[2]
		if first.ProcessEpoch != 1 || first.StopSequence <= 0 || !last.ObservedAt.After(first.ObservedAt) {
			return fmt.Errorf("invalid native stop observation interval")
		}
		var valid bool
		var observed json.RawMessage
		err := pool.QueryRow(ctx, `SELECT interrupted.status='interrupted' AND interrupted.result IS NULL
 AND interrupted.completion_save_id IS NULL AND successor.status='completed' AND successor.completion_save_id IS NOT NULL
 AND interrupted.process_epoch=$6 AND successor.process_epoch=interrupted.process_epoch
 AND successor.started_at>=interrupted.terminal_at AND successor.started_at>$7
 AND p.failure_recorded_at IS NULL AND p.stop_turn_id IS NULL AND p.stop_sequence>=$8
 AND (p.fenced_at IS NULL OR p.fenced_at>=successor.terminal_at)
 AND ask.status='cancelled' AND ask.process_epoch=interrupted.process_epoch
 AND NOT EXISTS(SELECT 1 FROM session_holds h WHERE h.environment_id=$1 AND h.session_id=$2),
 jsonb_build_object('sessionId',interrupted.session_id,'interruptedTurnId',interrupted.id,'successorTurnId',successor.id,
  'processEpoch',p.epoch,'stopSequence',p.stop_sequence,'interruptedTerminalAt',interrupted.terminal_at,
  'successorStartedAt',successor.started_at,'askCancelledAt',ask.cancelled_at,'processFencedAt',p.fenced_at)
 FROM turns interrupted JOIN turns successor ON successor.environment_id=interrupted.environment_id AND successor.session_id=interrupted.session_id AND successor.id=$4
 JOIN session_processes p ON (p.environment_id,p.session_id,p.epoch)=(interrupted.environment_id,interrupted.session_id,interrupted.process_epoch)
 JOIN turn_asks ask ON (ask.environment_id,ask.session_id,ask.turn_id)=(interrupted.environment_id,interrupted.session_id,interrupted.id) AND ask.id=$5
 WHERE interrupted.environment_id=$1 AND interrupted.session_id=$2 AND interrupted.id=$3`, env, stop.SessionID, stop.InterruptedTurnID, stop.SuccessorTurnID, stop.AskID, first.ProcessEpoch, last.ObservedAt, first.StopSequence).Scan(&valid, &observed)
		if err != nil {
			return err
		}
		if !valid {
			return fmt.Errorf("native stop settlement failed: %s", observed)
		}
		observations = append(observations, observed)
	}
	if counts["codex"] != 2 || counts["claude"] != 2 {
		return fmt.Errorf("native stop providers were not both exercised before and after restore")
	}
	raw, err := json.Marshal(observations)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(evidence, "native-stop-settlement.json"), raw, 0600)
}
