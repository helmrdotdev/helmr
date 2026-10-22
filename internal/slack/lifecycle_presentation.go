package slack

import (
	"context"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

func lifecycleButton(config ProjectionConfig, target controlTarget, action, label, actor, scope string, computerID uuid.UUID) (map[string]any, error) {
	value, err := encodeControl(config.ControlKey, controlEnvelope{Nonce: target.Thread, Action: action, Target: target, Actor: actor, Scope: scope, Computer: computerID, ExpiresAt: time.Now().UTC().Truncate(time.Hour).Add(25 * time.Hour).Unix()})
	if err != nil {
		return nil, err
	}
	button := map[string]any{"type": "button", "action_id": "helmr.lifecycle." + action, "text": plainText(label), "value": value}
	if action == "hibernate" || action == "confirm_close" {
		text := "Background tasks on this Computer will stop progressing until you wake it or send a new request. State is preserved when capture succeeds."
		if action == "confirm_close" {
			text = "Close this Session and the owned Sessions listed here? New messages will no longer be accepted. Accepted work follows the close policy. Wake cannot undo closure."
		}
		button["confirm"] = map[string]any{"title": plainText(label + "?"), "text": plainText(text), "confirm": plainText("Confirm"), "deny": plainText("Cancel")}
	}
	return button, nil
}

// Private help is rendered only after current linked-user authorization. Shared
// cards never disclose another Session's identity or issue scope confirmation.
func lifecyclePresentation(ctx context.Context, tx pgx.Tx, config ProjectionConfig, target controlTarget, action, actor string, private bool) (string, []any, error) {
	var computerID *uuid.UUID
	var status, policy string
	var idle *int64
	var muted bool
	if err := tx.QueryRow(ctx, `SELECT s.computer_id,s.status,s.suspension,s.close_after_idle_ms,t.input_muted FROM sessions s JOIN slack_threads t ON t.id=$3 AND t.front_session_id=s.id WHERE s.environment_id=$1 AND s.id=$2`, target.Environment, target.Session, target.Thread).Scan(&computerID, &status, &policy, &idle, &muted); err != nil {
		return "", nil, err
	}
	sleeping := false
	text := "Session " + status + "."
	if status == "open" {
		state := "Computer not yet running"
		if computerID != nil {
			var checkpoint, control, controlStatus, failure string
			var awake bool
			if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT status FROM checkpoints WHERE environment_id=$1 AND computer_id=$2 AND status IN ('capturing','sealed','ready','restoring','aborting') ORDER BY created_at DESC LIMIT 1),''),
 EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND status='active' AND fenced_at IS NULL),
 COALESCE((SELECT kind FROM computer_controls WHERE environment_id=$1 AND computer_id=$2 ORDER BY accepted_at DESC,id DESC LIMIT 1),''),
 COALESCE((SELECT status FROM computer_controls WHERE environment_id=$1 AND computer_id=$2 ORDER BY accepted_at DESC,id DESC LIMIT 1),''),
 COALESCE((SELECT error_code FROM computer_controls WHERE environment_id=$1 AND computer_id=$2 ORDER BY accepted_at DESC,id DESC LIMIT 1),'')`, target.Environment, *computerID).Scan(&checkpoint, &awake, &control, &controlStatus, &failure); err != nil {
				return "", nil, err
			}
			if awake {
				state = "Computer awake. Background work can continue"
			}
			switch checkpoint {
			case "ready":
				sleeping = true
				state = "Computer sleeping. Background work is paused; a new request wakes it"
			case "capturing", "sealed":
				state = "Hibernating…"
			case "restoring", "aborting":
				state = "Waking…"
			}
			if controlStatus == "accepted" {
				if control == "wake" {
					state = "Waking…"
				} else {
					state = "Hibernating…"
				}
			}
			if controlStatus == "failed" {
				state += ". Could not " + control + ". " + lifecycleFailureMessage(failure)
			}
		}
		suspension := "Automatic hibernation: off. "
		if policy == "after_turn" {
			suspension = "May hibernate after a completed Turn. "
		}
		text = state + ". " + suspension
		if idle == nil {
			text += "Automatic close: off."
		} else {
			text += fmt.Sprintf("Closes after %s without new user work; background tasks may be stopped.", (time.Duration(*idle) * time.Millisecond).String())
		}
		if muted {
			text += " Thread input is muted; an explicit Agent mention still sends work."
		}
	}
	blocks := []any{map[string]any{"type": "section", "text": plainText(text)}}
	var buttons []any
	add := func(action, label, scope string, id uuid.UUID) error {
		b, err := lifecycleButton(config, target, action, label, actor, scope, id)
		if err == nil {
			buttons = append(buttons, b)
		}
		return err
	}
	if status == "open" {
		if private && (action == "sleep" || action == "close") {
			var scope session.ExecutionScope
			var err error
			if action == "sleep" && computerID != nil {
				scope, err = session.ReadComputerExecutionScope(ctx, tx, target.Environment, *computerID)
			} else if action == "close" {
				scope, err = session.ReadSessionCloseScope(ctx, tx, target.Environment, target.Session)
			} else {
				return text, blocks, nil
			}
			if err != nil {
				return "", nil, err
			}
			members := append(append([]string{}, scope.Sessions...), scope.Commands...)
			detail := fmt.Sprintf("Affected scope: %d Sessions, %d active Commands.\n", len(scope.Sessions), len(scope.Commands))
			for i, id := range members {
				if i == 12 {
					detail += "Additional members are listed in Console.\n"
					break
				}
				detail += id + "\n"
			}
			blocks = append(blocks, map[string]any{"type": "section", "text": plainText(strings.TrimSpace(detail))})
			if action == "sleep" {
				err = add("hibernate", "Hibernate Computer", scope.Digest, *computerID)
			} else {
				err = add("confirm_close", "Close Session", scope.Digest, uuid.Nil())
			}
			if err != nil {
				return "", nil, err
			}
		} else {
			if computerID != nil {
				action, label := "sleep", "Hibernate Computer"
				if sleeping {
					action, label = "wake", "Wake Computer"
				}
				if err := add(action, label, "", uuid.Nil()); err != nil {
					return "", nil, err
				}
			}
			if private {
				muteAction, muteLabel := "mute", "Mute thread input"
				if muted {
					muteAction, muteLabel = "unmute", "Unmute thread input"
				}
				if err := add(muteAction, muteLabel, "", uuid.Nil()); err != nil {
					return "", nil, err
				}
				if err := add("close", "Close Session", "", uuid.Nil()); err != nil {
					return "", nil, err
				}
			} else if err := add("help", "More", "", uuid.Nil()); err != nil {
				return "", nil, err
			}
		}
	}
	if len(buttons) > 0 {
		blocks = append(blocks, map[string]any{"type": "actions", "elements": buttons})
	}
	if private && action == "help" {
		blocks = append(blocks, map[string]any{"type": "section", "text": plainText("!sleep: confirm Computer hibernation. !wake: resume background execution without a prompt. !close: confirm terminal Session closure. !mute / !unmute: change future thread input. !aside text: keep one message out of Agent input. These commands do not release Stop holds.")})
	}
	return text, blocks, nil
}

func lifecycleFailureMessage(code string) string {
	switch code {
	case "capture_refused":
		return "A consistent snapshot could not be made; review active work and try again"
	case "new_execution_requested":
		return "New work cancelled the hibernation request"
	case "capture_resumed":
		return "The Computer resumed before hibernation finished"
	case "execution_deadline_exceeded":
		return "The operation did not finish in time; review capacity and Computer state in Console"
	case "computer_deleted":
		return "The Computer was deleted"
	case "checkpoint_unavailable":
		return "The saved execution state is unavailable; view details in Console"
	default:
		return "Review the Computer state in Console before trying again"
	}
}
