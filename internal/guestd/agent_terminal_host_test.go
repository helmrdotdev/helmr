package guestd

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computerhost"
	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type terminalRelayMachine struct {
	vm.GuestControlMachine
	relay *agentRelay
}

func (m terminalRelayMachine) WithRunningGuestControl(ctx context.Context, _ vm.GuestControlStage, run func(context.Context) error) error {
	return run(ctx)
}
func (m terminalRelayMachine) OpenStream(context.Context) (vm.Stream, error) {
	host, guest := net.Pipe()
	go func() {
		defer guest.Close()
		if _, _, err := wire.ReadStreamFrameHeader(guest); err != nil {
			return
		}
		var attach agentv1.SessionAttach
		if err := frameio.ReadProtoFrameBounded(guest, maxAgentTransportFrameBytes, &attach); err != nil {
			return
		}
		if err := m.relay.attach(guest, &attach); err != nil {
			return
		}
		_ = m.relay.serve(guest, attach.AttachmentSequence)
	}()
	return host, nil
}

type terminalRelayClient struct {
	computerhost.AgentExecutionClient
	turn     string
	observed bool
	stopped  bool
}

func (c *terminalRelayClient) ObserveAgentTurn(_ context.Context, r workerapi.AgentTurnReceipt) (workerapi.AgentTurnAcknowledgment, error) {
	if r.TurnID != c.turn || string(r.Outcome) != `{"status":"completed","result":7}` {
		return workerapi.AgentTurnAcknowledgment{}, errors.New("wrong outcome")
	}
	c.observed = true
	return workerapi.AgentTurnAcknowledgment{Sequence: 1}, nil
}
func (c *terminalRelayClient) ObserveAgentStopped(context.Context, workerapi.AgentControlRequest) error {
	if !c.observed {
		return errors.New("physical stop preceded retained outcome")
	}
	c.stopped = true
	return nil
}
func (c *terminalRelayClient) RenewAgentAuthority(context.Context, workerapi.RuntimeSession) (workerapi.AgentAuthorityResponse, error) {
	return workerapi.AgentAuthorityResponse{AuthorityGeneration: 3, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func TestTerminalAgentHostDrainsOutcomeBeforePhysicalStop(t *testing.T) {
	session, _ := agentSessionFixture(t)
	turn := uuid.NewV7().String()
	session.turns[turn] = &agentTurnProof{sequence: 1, settled: true}
	session.terminal = true
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	relay := newAgentRelay(ctx, session, nil)
	identity := session.grant.Identity
	relay.enqueue(&agentv1.GuestSessionMessage{Identity: identity, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: identity, Event: &agentv1.ProgramEvent_DeliveryResult{DeliveryResult: &agentv1.DeliveryResult{DeliveryId: "turn:" + turn, Outcome: &agentv1.DeliveryResult_ValueJson{ValueJson: []byte(`{"status":"completed","result":7}`)}}}}}})
	relay.enqueue(&agentv1.GuestSessionMessage{Identity: identity, Message: &agentv1.GuestSessionMessage_Stopped{Stopped: &agentv1.SessionStopped{}}})
	go relay.sendLoop()
	connection, attached, err := computerhost.OpenAgentSession(ctx, terminalRelayMachine{relay: relay}, &agentv1.SessionAttach{Grant: session.grant, AttachmentSequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	client := &terminalRelayClient{turn: turn}
	done := make(chan error, 1)
	go func() {
		done <- computerhost.ServeExecutingAgentSession(ctx, connection, attached, "environment", client, func(context.Context, *agentv1.GuestSessionMessage) error { return errors.New("unexpected event") })
	}()
	// The real guest rejects commands after terminal. Both events can be released
	// only if the host verifies the outcome without writing to that closed runtime.
	waitAgentRelay(t, relay, func() bool { return relay.acknowledged == 2 })
	cancel()
	<-done
	if !client.observed || !client.stopped {
		t.Fatal("retained terminal sequence was not drained")
	}
}
