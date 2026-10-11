package computerhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/ids"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"strings"
	"time"
)

type AgentMessageClient interface {
	NextAgentMessage(context.Context, workerapi.AgentMessageRequest) (*workerapi.AgentMessageDispatch, error)
	ObserveAgentMessage(context.Context, workerapi.AgentMessageReceipt) error
}

// Called with the Turn owner locked. One durable callback remains outstanding
// until its receipt commits; replacement attachments reclaim that same identity.
func (owner *agentSessionTurns) dispatchMessageLocked(ctx context.Context) error {
	if owner.pendingMessage != "" {
		return nil
	}
	grant := owner.connection.CurrentGrant()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	next, err := owner.client.NextAgentMessage(ctx, workerapi.AgentMessageRequest{Session: owner.connection.runtimeSession(owner.environment, grant.GetComputerLeaseEpoch()), AttachmentSequence: int64(owner.connection.attachment), AuthorityGeneration: grant.GetAuthorityGeneration(), TurnID: owner.pending})
	var rejected interface{ SessionAuthorityRejected() bool }
	if errors.As(err, &rejected) && rejected.SessionAuthorityRejected() {
		return nil
	}
	if err != nil || next == nil {
		return err
	}
	if next.TurnID != owner.pending || ids.Validate(next.MessageID) != nil || !json.Valid(next.Input) {
		return errors.New("invalid message assignment")
	}
	owner.pendingMessage = "message:" + next.TurnID + ":" + next.MessageID
	return owner.connection.SendCommand(ctx, &agentv1.GuestCommand{Identity: owner.connection.identity, DeliveryId: owner.pendingMessage, Command: &agentv1.GuestCommand_Message{Message: &agentv1.MessageDelivery{TurnId: next.TurnID, MessageId: next.MessageID, InputJson: next.Input}}})
}

func (owner *agentSessionTurns) observeMessage(ctx context.Context, result *agentv1.DeliveryResult) error {
	parts := strings.Split(result.GetDeliveryId(), ":")
	if len(parts) != 3 || ids.Validate(parts[1]) != nil || ids.Validate(parts[2]) != nil {
		return errors.New("invalid message receipt identity")
	}
	reason := ""
	if failure := result.GetError(); failure != nil {
		switch failure.GetCode() {
		case "message_rejected", "turn_closed":
			reason = failure.GetCode()
		default:
			reason = "callback_failed"
		}
	} else if !bytes.Equal(bytes.TrimSpace(result.GetValueJson()), []byte("null")) {
		return errors.New("invalid message callback receipt")
	}
	grant := owner.connection.CurrentGrant()
	if err := owner.client.ObserveAgentMessage(ctx, workerapi.AgentMessageReceipt{Session: owner.connection.runtimeSession(owner.environment, grant.GetComputerLeaseEpoch()), AttachmentSequence: int64(owner.connection.attachment), TurnID: parts[1], MessageID: parts[2], RejectionReason: reason}); err != nil {
		return err
	}
	owner.mu.Lock()
	if owner.pendingMessage == result.GetDeliveryId() {
		owner.pendingMessage = ""
	}
	owner.mu.Unlock()
	return nil
}
