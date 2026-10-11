package runtimemcp

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Waiting lives outside the relay operation slot so independent mutations and
// cancellation reconciliation can progress while the observed Turn is pending.
func waitForTurn(ctx context.Context, invoke Invoke, input waitInput) (json.RawMessage, error) {
	if err := validate(input); err != nil {
		return nil, err
	}
	expired := errors.New("turn observation timed out")
	observation, cancel := context.WithTimeoutCause(ctx, time.Duration(input.TimeoutMS)*time.Millisecond, expired)
	defer cancel()
	body, _ := json.Marshal(turnInput{SessionID: input.SessionID, TurnID: input.TurnID})
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if observation.Err() != nil {
			return json.RawMessage(`{"status":"timeout"}`), nil
		}
		raw, err := invoke(observation, "inspect_turn", body)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if context.Cause(observation) == expired {
			return json.RawMessage(`{"status":"timeout"}`), nil
		}
		if err != nil {
			return nil, err
		}
		var state struct {
			ID               string          `json:"id"`
			SessionID        string          `json:"sessionId"`
			Status           string          `json:"status"`
			WaitBlocked      string          `json:"waitBlocked"`
			Result           json.RawMessage `json:"result,omitempty"`
			Response         json.RawMessage `json:"response,omitempty"`
			Error            json.RawMessage `json:"error,omitempty"`
			PayloadExpiredAt *time.Time      `json:"payloadExpiredAt,omitempty"`
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			return nil, err
		}
		if state.ID != input.TurnID || state.SessionID != input.SessionID {
			return nil, errors.New("turn response changed identity")
		}
		switch state.Status {
		case "queued", "running", "finalizing":
			if state.Result != nil || state.Response != nil || state.Error != nil || state.PayloadExpiredAt != nil {
				return nil, errors.New("unfinished Turn published an outcome")
			}
			if state.WaitBlocked != "" {
				if state.Status != "queued" || state.WaitBlocked != "capacity_wait_blocked" {
					return nil, errors.New("invalid Turn wait dependency")
				}
				return nil, errors.New("capacity_wait_blocked: waiting depends on releasing the caller's retained Computer allocation")
			}
		case "completed", "failed", "interrupted", "cancelled":
			if state.Status != "completed" && (state.Result != nil || state.Response != nil) {
				return nil, errors.New("unsuccessful Turn published a result")
			}
			if state.PayloadExpiredAt != nil && (state.Result != nil || state.Response != nil) {
				return nil, errors.New("expired Turn published payloads")
			}
			outcome := map[string]any{"status": state.Status}
			if state.Result != nil {
				outcome["result"] = state.Result
			}
			if state.Response != nil {
				outcome["response"] = state.Response
			}
			if state.Error != nil {
				outcome["error"] = state.Error
			}
			if state.PayloadExpiredAt != nil {
				outcome["payloadExpiredAt"] = state.PayloadExpiredAt
			}
			return json.Marshal(map[string]any{"status": "settled", "outcome": outcome})
		default:
			return nil, errors.New("invalid Turn observation status")
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-observation.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}
