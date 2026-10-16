//go:build linux && computerproof

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Explicit interruption makes the ordinary stop reachable while Sync is blocked.
// A real server deadline or guest failure must settle only this Session. Staging
// stays unavailable until that failure; recovery must retain this Computer's disk.
func proveNativePressureStopFailure(ctx context.Context, pool *pgxpool.Pool, apiClient *client.Client, env uuid.UUID, started api.CreateSessionResponse, output api.AgentSessionEvent, gate *authorityLossGate, worker *nativeWorkerProcess, proof map[string]any) error {
	type state struct {
		Computer                 string
		LeaseEpoch, ProcessEpoch int64
		Pressure, Failed, Fenced bool
		LeaseStatus, TurnStatus  string
	}
	read := func() (state, error) {
		var s state
		err := pool.QueryRow(ctx, `SELECT p.computer_id::text,p.computer_lease_epoch,p.epoch,l.storage_pressure_since IS NOT NULL,
 p.failure_recorded_at IS NOT NULL,p.fenced_at IS NOT NULL,l.status,t.status
 FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch)
 JOIN turns t ON (t.environment_id,t.session_id,t.process_epoch)=(p.environment_id,p.session_id,p.epoch)
 WHERE p.environment_id=$1 AND p.session_id=$2 AND t.id=$3`, env, started.SessionID, started.Turn.TurnID).
			Scan(&s.Computer, &s.LeaseEpoch, &s.ProcessEpoch, &s.Pressure, &s.Failed, &s.Fenced, &s.LeaseStatus, &s.TurnStatus)
		return s, err
	}
	var before state
	if err := awaitLossCondition(ctx, func() (bool, error) {
		select {
		case <-worker.done:
			return false, fmt.Errorf("worker ended: %v", worker.err)
		default:
		}
		var err error
		before, err = read()
		return before.Pressure && gate.blockedObjects.Load() > 0, err
	}); err != nil {
		return err
	}
	if before.Failed || before.Fenced || before.LeaseStatus != "active" || before.TurnStatus != "running" {
		return fmt.Errorf("invalid initial pressure: %+v", before)
	}
	proof["blockedStopBefore"] = before
	if _, err := apiClient.InterruptSessionTurn(ctx, started.SessionID, started.Turn.TurnID, api.InterruptTurnRequest{IdempotencyKey: "pressure-stop"}, client.EnvironmentScopeOptions{}); err != nil {
		return err
	}
	requestedAt := time.Now()
	var failed state
	stopCtx, cancel := context.WithTimeout(ctx, 70*time.Second)
	defer cancel()
	if err := awaitLossCondition(stopCtx, func() (bool, error) {
		var err error
		failed, err = read()
		return failed.Failed, err
	}); err != nil {
		return err
	}
	if failed.Computer != before.Computer || failed.LeaseEpoch != before.LeaseEpoch || failed.ProcessEpoch != before.ProcessEpoch || failed.LeaseStatus != "active" || !failed.Pressure || failed.TurnStatus != "interrupted" {
		return fmt.Errorf("Session failure changed shared ownership: %+v", failed)
	}
	proof["blockedStopFailure"] = map[string]any{"state": failed, "elapsedMillis": time.Since(requestedAt).Milliseconds(), "observedAt": time.Now().UTC()}
	next, err := apiClient.EnqueueSession(ctx, started.SessionID, api.EnqueueSessionRequest{Input: json.RawMessage(`[{"type":"text","text":"\"verify-pressure\""}]`), IdempotencyKey: "pressure-reconstruct"}, client.EnvironmentScopeOptions{})
	if err != nil {
		return err
	}
	queued, err := apiClient.RetrieveSessionTurn(ctx, started.SessionID, next.TurnID, client.EnvironmentScopeOptions{})
	if err != nil || queued.Status != "queued" {
		return fmt.Errorf("failed Session admitted queued work: %s %v", queued.Status, err)
	}
	gate.blockObjects.Store(false)
	proof["blockedStopStorageReleasedAt"] = time.Now().UTC()
	if err := awaitLossCondition(ctx, func() (bool, error) { s, err := read(); return s.Fenced, err }); err != nil {
		return err
	}
	var held bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM session_processes WHERE environment_id=$1 AND session_id=$2)=1
 AND (SELECT status='queued' FROM turns WHERE environment_id=$1 AND id=$3)`, env, started.SessionID, next.TurnID).Scan(&held); err != nil || !held {
		return fmt.Errorf("replacement began before failure acknowledgement: %t %v", held, err)
	}
	view, err := apiClient.RetrieveSession(ctx, started.SessionID, client.EnvironmentScopeOptions{})
	if err != nil {
		return err
	}
	// Pressure clears independently. The runtime-failure acknowledgement must remain.
	var failureHold string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM session_holds WHERE environment_id=$1 AND session_id=$2 AND released_at IS NULL AND kind='control' AND scope='local' AND issuer_kind='system'`, env, started.SessionID).Scan(&failureHold); err != nil {
		return err
	}
	found := false
	for _, hold := range view.Holds {
		found = found || hold.ID == failureHold
	}
	if !found {
		return errors.New("Session failure acknowledgement was not exposed")
	}
	if _, err := apiClient.ResumeSession(ctx, started.SessionID, api.ResumeSessionRequest{HoldID: failureHold, IdempotencyKey: "pressure-acknowledge-failure"}, client.EnvironmentScopeOptions{}); err != nil {
		return err
	}
	var completed api.AgentTurn
	if err := awaitLossCondition(ctx, func() (bool, error) {
		var err error
		completed, err = apiClient.RetrieveSessionTurn(ctx, started.SessionID, next.TurnID, client.EnvironmentScopeOptions{})
		if err == nil && (completed.Status == "failed" || completed.Status == "interrupted" || completed.Status == "cancelled") {
			return false, fmt.Errorf("reconstructed Turn ended %s: %v", completed.Status, completed.Error)
		}
		return completed.Status == "completed", err
	}); err != nil {
		return err
	}
	var parts []struct{ Type, Text string }
	var expected, actual struct {
		FirstChunkDigest string `json:"firstChunkDigest"`
	}
	if err := json.Unmarshal(output.Data, &parts); err != nil {
		return err
	}
	if len(parts) != 1 || parts[0].Type != "text" {
		return errors.New("pressure output is not one text record")
	}
	if err := json.Unmarshal([]byte(parts[0].Text), &expected); err != nil {
		return err
	}
	if err := json.Unmarshal(completed.Result, &actual); err != nil {
		return err
	}
	if len(expected.FirstChunkDigest) != 64 || actual.FirstChunkDigest != expected.FirstChunkDigest {
		return errors.New("Session reconstruction lost retained pressure data")
	}
	var valid bool
	err = pool.QueryRow(ctx, `SELECT p.computer_id=$3 AND p.computer_lease_epoch=$4 AND p.epoch>$5
 AND EXISTS(SELECT 1 FROM session_processes old WHERE old.environment_id=$1 AND old.session_id=$2 AND old.epoch=$5 AND old.fenced_at IS NOT NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$3 AND epoch<>$4)
 AND (SELECT count(*) FROM session_events WHERE environment_id=$1 AND turn_id=$6 AND kind='turn.running')=1
 AND (SELECT count(*) FROM session_events WHERE environment_id=$1 AND turn_id=$6 AND kind='turn.interrupted')=1
 FROM session_processes p WHERE p.environment_id=$1 AND p.session_id=$2 ORDER BY p.epoch DESC LIMIT 1`, env, started.SessionID, before.Computer, before.LeaseEpoch, before.ProcessEpoch, started.Turn.TurnID).Scan(&valid)
	if err != nil || !valid {
		return fmt.Errorf("Session-local reconstruction/fencing/no-replay proof: %t %v", valid, err)
	}
	proof["blockedStopReconstructed"] = map[string]any{"turnId": next.TurnID, "sameComputerAndLease": true, "retainedFirstChunkDigest": actual.FirstChunkDigest, "observedAt": time.Now().UTC()}
	return nil
}
