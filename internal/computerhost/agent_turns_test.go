package computerhost

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type turnExecutionTestClient struct {
	AgentControlClient
	nextMessage    func(context.Context, workerapi.AgentMessageRequest) (*workerapi.AgentMessageDispatch, error)
	observeMessage func(context.Context, workerapi.AgentMessageReceipt) error
	next           func(context.Context, workerapi.AgentTurnRequest) (*workerapi.AgentTurnDispatch, error)
	observe        func(context.Context, workerapi.AgentTurnReceipt) (workerapi.AgentTurnAcknowledgment, error)
}

func (c *turnExecutionTestClient) NextAgentTurn(ctx context.Context, r workerapi.AgentTurnRequest) (*workerapi.AgentTurnDispatch, error) {
	return c.next(ctx, r)
}
func (c *turnExecutionTestClient) ObserveAgentTurn(ctx context.Context, r workerapi.AgentTurnReceipt) (workerapi.AgentTurnAcknowledgment, error) {
	return c.observe(ctx, r)
}

func TestAgentTurnRetiresBeforeDispatchingNext(t *testing.T) {
	turn := uuid.NewV7().String()
	grant := sessionTransportGrant()
	created := time.Now().UTC()
	guestDone := make(chan error, 1)
	machine := sessionTransportMachine(t, func(stream net.Conn, request *agentv1.SessionAttach) {
		read := func() (*agentv1.HostSessionMessage, error) {
			var message agentv1.HostSessionMessage
			err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &message)
			return &message, err
		}
		dispatch, err := read()
		if err != nil {
			guestDone <- err
			return
		}
		if dispatch.GetCommand().GetDeliveryId() != "turn:"+turn || dispatch.GetCommand().GetDispatch().GetCreatedAt() != created.Format(time.RFC3339Nano) || dispatch.GetCommand().GetDispatch().GetSequence() != 3 {
			guestDone <- errors.New("dispatch metadata changed")
			return
		}
		terminal := &agentv1.DeliveryResult{DeliveryId: "turn:" + turn, Outcome: &agentv1.DeliveryResult_ValueJson{ValueJson: []byte(`{"status":"completed","result":7}`)}}
		if err := frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: grant.Identity, AttachmentSequence: request.AttachmentSequence, EventSequence: 1, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: grant.Identity, Event: &agentv1.ProgramEvent_DeliveryResult{DeliveryResult: terminal}}}}); err != nil {
			guestDone <- err
			return
		}
		retire, err := read()
		if err != nil {
			guestDone <- err
			return
		}
		if retire.GetCommand().GetDeliveryId() != "turn-ack:"+turn || retire.GetCommand().GetAcknowledged().GetSequence() != 3 {
			guestDone <- errors.New("terminal event released before retirement command")
			return
		}
		ack, err := read()
		if err != nil {
			guestDone <- err
			return
		}
		if ack.GetAcknowledged().GetThroughSequence() != 1 {
			guestDone <- errors.New("terminal event not acknowledged")
			return
		}
		retired := &agentv1.DeliveryResult{DeliveryId: "turn-ack:" + turn, Outcome: &agentv1.DeliveryResult_ValueJson{ValueJson: []byte("null")}}
		guestDone <- frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: grant.Identity, AttachmentSequence: request.AttachmentSequence, EventSequence: 2, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: grant.Identity, Event: &agentv1.ProgramEvent_DeliveryResult{DeliveryResult: retired}}}})
	})
	connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	calls := 0
	client := &turnExecutionTestClient{next: func(_ context.Context, r workerapi.AgentTurnRequest) (*workerapi.AgentTurnDispatch, error) {
		calls++
		if r.AttachmentSequence != 4 || r.AuthorityGeneration != 3 || r.Session.ComputerLeaseEpoch != 2 {
			t.Fatalf("wrong dispatch authority: %+v", r)
		}
		if calls > 1 {
			return nil, nil
		}
		return &workerapi.AgentTurnDispatch{TurnID: turn, Sequence: 3, CreatedAt: created, Input: json.RawMessage(`[{"type":"text","text":"{\"message\":\"run\"}"}]`), Source: workerapi.AgentTurnSource{Kind: "user"}}, nil
	}, observe: func(_ context.Context, r workerapi.AgentTurnReceipt) (workerapi.AgentTurnAcknowledgment, error) {
		if r.TurnID != turn || r.AttachmentSequence != 4 || string(r.Outcome) != `{"status":"completed","result":7}` {
			t.Fatalf("changed terminal receipt: %+v", r)
		}
		return workerapi.AgentTurnAcknowledgment{Sequence: 3}, nil
	}}
	owner := &agentSessionTurns{connection: connection, client: client, environment: "environment"}
	if err := owner.dispatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	terminal, err := connection.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if handled, err := owner.observe(t.Context(), terminal); !handled || err != nil {
		t.Fatalf("terminal: %v %v", handled, err)
	}
	if err := owner.dispatch(t.Context()); err != nil || calls != 1 {
		t.Fatalf("dispatched before retirement: %d %v", calls, err)
	}
	if err := connection.Acknowledge(t.Context(), terminal.GetEventSequence()); err != nil {
		t.Fatal(err)
	}
	retired, err := connection.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if handled, err := owner.observe(t.Context(), retired); !handled || err != nil {
		t.Fatalf("retirement: %v %v", handled, err)
	}
	if err := <-guestDone; err != nil {
		t.Fatal(err)
	}
	if err := owner.dispatch(t.Context()); err != nil || calls != 2 {
		t.Fatalf("next dispatch blocked: %d %v", calls, err)
	}
}

func TestAgentTurnReattachmentWaitsForPendingReceipt(t *testing.T) {
	owner := &agentSessionTurns{connection: &AgentSessionConnection{grant: sessionTransportGrant(), identity: sessionTransportGrant().Identity}, pending: uuid.NewV7().String(), client: &turnExecutionTestClient{next: func(context.Context, workerapi.AgentTurnRequest) (*workerapi.AgentTurnDispatch, error) {
		t.Fatal("pending Turn was bypassed")
		return nil, nil
	}}}
	if err := owner.dispatch(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestAgentTurnFailureRetainedUntilDurableReport(t *testing.T) {
	grant := sessionTransportGrant()
	done := make(chan error, 1)
	machine := sessionTransportMachine(t, func(stream net.Conn, request *agentv1.SessionAttach) {
		message := &agentv1.GuestSessionMessage{Identity: grant.Identity, AttachmentSequence: request.AttachmentSequence, EventSequence: 1, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: grant.Identity, Event: &agentv1.ProgramEvent_Failed{Failed: &agentv1.SessionFailed{Code: "setup_failed", Message: "setup failed"}}}}}
		if err := frameio.WriteProtoFrame(stream, message); err != nil {
			done <- err
			return
		}
		var ack agentv1.HostSessionMessage
		if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &ack); err != nil {
			done <- err
			return
		}
		if ack.GetAcknowledged().GetThroughSequence() != 1 {
			done <- errors.New("failure event was not acknowledged")
			return
		}
		done <- nil
	})
	connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	message, err := connection.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reports := 0
	client := &turnExecutionTestClient{AgentControlClient: &terminalControlClient{failure: func(context.Context, workerapi.AgentControlRequest) error {
		reports++
		if reports == 1 {
			return errors.New("receipt uncertain")
		}
		return nil
	}}}
	owner := &agentSessionTurns{connection: connection, client: client, environment: "environment"}
	for attempt := 1; attempt <= 2; attempt++ {
		handled, err := owner.observe(t.Context(), message)
		var failure SessionControlFailedError
		if !handled || !errors.As(err, &failure) || (failure.FailureError == nil) != (attempt == 2) {
			t.Fatalf("failure disposition %d: %v %v", attempt, handled, err)
		}
		if attempt == 1 {
			select {
			case <-done:
				t.Fatal("uncertain failure report released retained event")
			default:
			}
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func (c *turnExecutionTestClient) NextAgentMessage(ctx context.Context, request workerapi.AgentMessageRequest) (*workerapi.AgentMessageDispatch, error) {
	if c.nextMessage != nil {
		return c.nextMessage(ctx, request)
	}
	return nil, nil
}
func (c *turnExecutionTestClient) ObserveAgentMessage(ctx context.Context, receipt workerapi.AgentMessageReceipt) error {
	if c.observeMessage != nil {
		return c.observeMessage(ctx, receipt)
	}
	return nil
}

func TestAgentMessageWaitsForDurableCallbackReceipt(t *testing.T) {
	turn, id := uuid.NewV7().String(), uuid.NewV7().String()
	grant := sessionTransportGrant()
	delivered := make(chan *agentv1.GuestCommand, 1)
	machine := sessionTransportMachine(t, func(stream net.Conn, request *agentv1.SessionAttach) {
		var message agentv1.HostSessionMessage
		if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &message); err != nil {
			delivered <- nil
			return
		}
		delivered <- message.GetCommand()
		<-t.Context().Done()
	})
	connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	calls, observed := 0, 0
	client := &turnExecutionTestClient{
		nextMessage: func(_ context.Context, request workerapi.AgentMessageRequest) (*workerapi.AgentMessageDispatch, error) {
			calls++
			if request.TurnID != turn || request.AttachmentSequence != 4 || request.AuthorityGeneration != grant.AuthorityGeneration {
				t.Fatalf("wrong message authority: %+v", request)
			}
			return &workerapi.AgentMessageDispatch{TurnID: turn, MessageID: id, Input: json.RawMessage(`[{"type":"text","text":"steer"}]`)}, nil
		},
		observeMessage: func(_ context.Context, receipt workerapi.AgentMessageReceipt) error {
			observed++
			if receipt.TurnID != turn || receipt.MessageID != id || receipt.RejectionReason != "message_rejected" {
				t.Fatalf("wrong receipt: %+v", receipt)
			}
			if observed == 1 {
				return errors.New("lost reply")
			}
			return nil
		},
	}
	owner := &agentSessionTurns{connection: connection, client: client, environment: "environment", pending: turn}
	if err := owner.dispatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	command := <-delivered
	if command.GetMessage().GetMessageId() != id || command.GetMessage().GetTurnId() != turn {
		t.Fatalf("changed delivery: %v", command)
	}
	if err := owner.dispatch(t.Context()); err != nil || calls != 1 {
		t.Fatalf("duplicate callback dispatched: %d %v", calls, err)
	}
	result := &agentv1.DeliveryResult{DeliveryId: command.DeliveryId, Outcome: &agentv1.DeliveryResult_Error{Error: &agentv1.OperationError{Code: "message_rejected"}}}
	if err := owner.observeMessage(t.Context(), result); err == nil || owner.pendingMessage == "" {
		t.Fatal("lost receipt released callback")
	}
	if err := owner.observeMessage(t.Context(), result); err != nil || owner.pendingMessage != "" {
		t.Fatalf("committed receipt not released: %v", err)
	}
}
