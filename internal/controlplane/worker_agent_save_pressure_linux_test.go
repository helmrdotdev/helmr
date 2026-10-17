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
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Fault only object admission at the fixture's HTTP boundary. Guest writes,
// fsync, kernel NBD, pressure reporting and recovery use the real worker path.
// The short wait stays within the grant margin; terminal owner loss is tested
// separately while a later Turn is blocked by staging pressure.
func proveNativeSavePressure(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, started api.CreateSessionResponse, gate *authorityLossGate, worker *nativeWorkerProcess, proof map[string]any) error {
	type observation struct {
		Computer      string
		LeaseEpoch    int64
		ProcessEpoch  int64
		PressureSince *time.Time
		LeaseStatus   string
		ProcessStatus string
		TurnStatus    string
	}
	read := func() (observation, error) {
		var value observation
		err := pool.QueryRow(ctx, `SELECT p.computer_id::text,p.computer_lease_epoch,p.epoch,
 l.storage_pressure_since,l.status,p.status,t.status FROM session_processes p
 JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch)
 JOIN turns t ON (t.environment_id,t.session_id)=(p.environment_id,p.session_id)
 WHERE p.environment_id=$1 AND p.session_id=$2 AND t.id=$3 ORDER BY p.epoch DESC LIMIT 1`, env, started.SessionID, started.Turn.TurnID).
			Scan(&value.Computer, &value.LeaseEpoch, &value.ProcessEpoch, &value.PressureSince, &value.LeaseStatus, &value.ProcessStatus, &value.TurnStatus)
		return value, err
	}
	var before observation
	if err := awaitLossCondition(ctx, func() (bool, error) {
		select {
		case <-worker.done:
			return false, fmt.Errorf("worker exited before pressure: %v", worker.err)
		default:
		}
		value, err := read()
		if err != nil {
			return false, err
		}
		before = value
		return value.PressureSince != nil && gate.blockedObjects.Load() > 0, nil
	}); err != nil {
		return err
	}
	if before.LeaseStatus != "active" || before.ProcessStatus != "ready" || before.TurnStatus != "running" {
		return fmt.Errorf("unexpected state at pressure: %+v", before)
	}
	observedAt := time.Now()
	proof["pressureBefore"] = before
	proof["pressureObservedAt"] = observedAt.UTC()
	// This is a short-stall continuity check, not an arbitrary-duration promise.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-worker.done:
		return fmt.Errorf("worker exited during pressure: %v", worker.err)
	case <-timer.C:
	}
	after, err := read()
	if err != nil {
		return err
	}
	if after.Computer != before.Computer || after.LeaseEpoch != before.LeaseEpoch || after.ProcessEpoch != before.ProcessEpoch || after.PressureSince == nil || !after.PressureSince.Equal(*before.PressureSince) || after.LeaseStatus != "active" || after.ProcessStatus != "ready" || after.TurnStatus != "running" {
		return errors.New("pressure changed live ownership or failed the blocked Turn")
	}
	proof["pressureAfterWait"] = after
	proof["pressureWaitMillis"] = time.Since(observedAt).Milliseconds()
	proof["objectAdmissionFailures"] = gate.blockedObjects.Load()
	gate.blockObjects.Store(false)
	proof["pressureStorageReleasedAt"] = time.Now().UTC()
	return nil
}

func proveNativeDiskRecovery(ctx context.Context, pool *pgxpool.Pool, apiClient *client.Client, env uuid.UUID, original api.CreateSessionResponse, save workerapi.AgentSave, published bool, originalResult json.RawMessage, proof map[string]any) error {
	view, err := apiClient.RetrieveSession(ctx, original.SessionID, client.EnvironmentScopeOptions{})
	if err != nil {
		return err
	}
	if view.ComputerID == nil {
		return errors.New("lost Session has no Computer")
	}
	var hold string
	for _, h := range view.Holds {
		if h.Kind == "recovery_gap" {
			if hold != "" || h.RecoveryGap == nil || len(h.RecoveryGap.AffectedTurnIDs) != 1 || h.RecoveryGap.AffectedTurnIDs[0] != original.Turn.TurnID {
				return errors.New("unexpected recovery gap identity or affected Turn count")
			}
			hold = h.ID
		}
	}
	if (hold == "") != published || len(view.Holds) != map[bool]int{true: 0, false: 1}[published] {
		return errors.New("recovery hold does not match published coverage")
	}
	// Snapshot the selected recovery head before a successor can publish another save.
	var recoverySaveID *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT recovery_save_id FROM computers WHERE environment_id=$1 AND id=$2`, env, *view.ComputerID).Scan(&recoverySaveID); err != nil {
		return err
	}
	next, err := apiClient.EnqueueSession(ctx, original.SessionID, api.EnqueueSessionRequest{Input: json.RawMessage(`[{"type":"text","text":"\"recover-check\""}]`), IdempotencyKey: "loss-recover-original"}, client.EnvironmentScopeOptions{})
	if err != nil {
		return err
	}
	if hold != "" {
		queued, err := apiClient.RetrieveSessionTurn(ctx, original.SessionID, next.TurnID, client.EnvironmentScopeOptions{})
		if err != nil || queued.Status != "queued" {
			return fmt.Errorf("gap did not hold successor: %s %v", queued.Status, err)
		}
		if _, err := apiClient.ResumeSession(ctx, original.SessionID, api.ResumeSessionRequest{HoldID: hold, IdempotencyKey: "loss-acknowledge-gap"}, client.EnvironmentScopeOptions{}); err != nil {
			return err
		}
	}
	var completed api.AgentTurn
	if err := awaitLossCondition(ctx, func() (bool, error) {
		var err error
		completed, err = apiClient.RetrieveSessionTurn(ctx, original.SessionID, next.TurnID, client.EnvironmentScopeOptions{})
		if err != nil {
			return false, err
		}
		if completed.Status == "failed" || completed.Status == "interrupted" || completed.Status == "cancelled" {
			return false, fmt.Errorf("recovered Turn ended as %s", completed.Status)
		}
		return completed.Status == "completed", nil
	}); err != nil {
		return err
	}
	type marker struct {
		Nonce string `json:"nonce"`
		Count int64  `json:"count"`
	}
	var before marker
	var after struct {
		marker
		Retained *marker `json:"retained"`
	}
	if err := json.Unmarshal(originalResult, &before); err != nil {
		return err
	}
	if err := json.Unmarshal(completed.Result, &after); err != nil {
		return err
	}
	if after.Nonce == "" || after.Nonce == before.Nonce || after.Count != 0 {
		return errors.New("recovery did not create a fresh Session process")
	}
	if published && (after.Retained == nil || *after.Retained != before) {
		return errors.New("covered recovery lost the published guest marker")
	}
	var valid bool
	err = pool.QueryRow(ctx, `SELECT p.computer_id=$3 AND p.computer_lease_epoch>$4 AND p.epoch>1
 AND l.disk_only_recovery AND l.restored_from_save_id IS NOT DISTINCT FROM $5::uuid
 AND (SELECT count(*) FROM session_events WHERE environment_id=$1 AND turn_id=$6 AND kind='turn.running')=1
 AND (SELECT count(*) FROM session_events WHERE environment_id=$1 AND turn_id=$6 AND kind='turn.completed')=1
 FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch)
 WHERE p.environment_id=$1 AND p.session_id=$2 ORDER BY p.epoch DESC LIMIT 1`, env, original.SessionID, *view.ComputerID, save.LeaseEpoch, recoverySaveID, original.Turn.TurnID).Scan(&valid)
	if err != nil || !valid {
		return fmt.Errorf("disk recovery identity or no-replay evidence: %t %v", valid, err)
	}
	proof["recoveredOriginalComputer"] = map[string]any{"computerId": *view.ComputerID, "turnId": next.TurnID, "acknowledgedHoldId": hold, "recoverySaveId": recoverySaveID, "result": completed.Result, "observedAt": time.Now().UTC()}
	return nil
}
