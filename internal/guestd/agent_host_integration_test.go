package guestd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/computerhost"
	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

type agentHostTestMachine struct {
	registry       *computerOperationRegistry
	stage          vm.GuestControlStage
	loseActivation bool
}

func (*agentHostTestMachine) Stream() vm.Stream           { return nil }
func (*agentHostTestMachine) Wait(context.Context) error  { return nil }
func (*agentHostTestMachine) Close(context.Context) error { return nil }
func (m *agentHostTestMachine) WithRunningGuestControl(ctx context.Context, stage vm.GuestControlStage, exchange func(context.Context) error) error {
	m.stage = stage
	defer func() { m.stage = vm.GuestControlOrdinary }()
	return exchange(ctx)
}
func (m *agentHostTestMachine) OpenStream(ctx context.Context) (vm.Stream, error) {
	host, guest := net.Pipe()
	var connection programConnection = guest
	if m.stage == vm.GuestControlActivation && m.loseActivation {
		m.loseActivation = false
		connection = agentLostReplyConnection{Conn: guest}
	}
	go func() { defer guest.Close(); _ = handleConnection(ctx, connection, slog.Default(), m.registry) }()
	return host, nil
}

type agentLostReplyConnection struct{ net.Conn }

func (c agentLostReplyConnection) Write([]byte) (int, error) {
	_ = c.Close()
	return 0, io.ErrClosedPipe
}

type agentHostTestOwner struct {
	machine      *agentHostTestMachine
	connections  map[string]vm.Stream
	sequences    map[string]uint64
	committed    bool
	advanceClock bool
}

func (owner *agentHostTestOwner) AttachSessions(ctx context.Context, installation *agentv1.ComputerSessionInstallation) error {
	if owner.advanceClock {
		entry := owner.machine.registry.agentCapture.entry
		entry.authorityClock.Store(&computerAuthorityClock{anchor: time.Now(), authority: time.Now().Add(24 * time.Hour).UnixNano()})
		owner.advanceClock = false
	}
	for _, installed := range installation.GetGrants() {
		id := installed.GetIdentity().GetSessionId()
		if owner.connections[id] != nil {
			continue
		}
		grant := proto.Clone(installed).(*agentv1.SessionGrant)
		// Current CP authority renews the same retained process. It does not rewrite
		// the immutable installation receipt or reconstruct setup.
		grant.AuthorityGeneration++
		grant.ExpiresAtUnixNano = owner.machine.registry.agentCapture.entry.authorityNow().Add(time.Hour).UnixNano()
		owner.sequences[id]++
		stream, err := owner.machine.OpenStream(ctx)
		if err != nil {
			return err
		}
		if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{Type: wire.StreamTypeAgentSession}, 0); err != nil {
			stream.Close()
			return err
		}
		if err := frameio.WriteProtoFrame(stream, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: owner.sequences[id]}); err != nil {
			stream.Close()
			return err
		}
		var attached agentv1.GuestSessionMessage
		if err := frameio.ReadProtoFrameBounded(stream, maxAgentTransportFrameBytes, &attached); err != nil {
			stream.Close()
			return err
		}
		if attached.GetAttached() == nil {
			stream.Close()
			return errors.New("Session did not attach")
		}
		owner.connections[id] = stream
	}
	return nil
}
func (owner *agentHostTestOwner) ValidateTarget(_ context.Context, _ *agentv1.ComputerSessionInstallation, observed *agentv1.ComputerSessionReceipt) error {
	if owner.committed && !observed.GetInstalled() {
		return errors.New("committed continuation cannot reuse old image")
	}
	return nil
}
func (owner *agentHostTestOwner) PrepareActivation(_ context.Context, installation *agentv1.ComputerSessionInstallation, receipt *agentv1.ComputerSessionReceipt) error {
	if !receipt.GetInstalled() || receipt.GetDesiredVersion() != installation.GetDesiredVersion() {
		return errors.New("installed receipt required")
	}
	owner.committed = true
	return nil
}
func (owner *agentHostTestOwner) CurrentControls(_ context.Context, _ *agentv1.ComputerSessionInstallation, _ *agentv1.ComputerSessionReceipt) (*agentv1.ComputerSessionControls, error) {
	return currentAgentComputerTestControls(owner.machine.registry), nil
}

// This fixture makes no durable control changes while its process is parked.
func (*agentHostTestOwner) ReconcileControls(context.Context, *agentv1.ComputerSessionInstallation, *agentv1.ComputerSessionReceipt) error {
	return nil
}

func TestAgentComputerHostWireRetainsProcessesAndRenewsExpiredAuthority(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired-grants", true: "lost-activation"}[lostReply], func(t *testing.T) {
			registry, capture, processes := agentComputerFixture(t)
			registry.agentSessions["peer"].session.held = true
			if _, err := registry.captureAgentComputer(t.Context(), capture); err != nil {
				t.Fatal(err)
			}
			installation := agentComputerInstallation(registry, capture, false)
			machine := &agentHostTestMachine{registry: registry, loseActivation: lostReply}
			owner := &agentHostTestOwner{machine: machine, connections: map[string]vm.Stream{}, sequences: map[string]uint64{}, advanceClock: true}
			t.Cleanup(func() {
				for _, stream := range owner.connections {
					stream.Close()
				}
			})
			receipt, err := computerhost.ContinueAgentComputer(t.Context(), machine, installation, owner)
			if lostReply {
				if err == nil || !registry.agentCapture.activated {
					t.Fatalf("lost activated reply: %v", err)
				}
				receipt, err = computerhost.ContinueAgentComputer(t.Context(), machine, installation, owner)
			}
			if err != nil || !receipt.GetActivated() {
				t.Fatalf("real host/guest continuation: %v %v", receipt, err)
			}
			for _, process := range processes {
				if process.physicallyFrozen {
					t.Fatal("retained process stayed frozen")
				}
			}
			if !registry.agentSessions["peer"].session.held {
				t.Fatal("continuation released peer hold")
			}
			for _, sequence := range owner.sequences {
				if sequence != 1 {
					t.Fatal("retry replaced valid attachment")
				}
			}
		})
	}
}

func TestAgentSessionAbsenceRequiresExactComputerAuthority(t *testing.T) {
	session, _ := agentSessionFixture(t)
	registry := newComputerOperationRegistry()
	registry.entries[session.entry.computerInstanceID] = session.entry
	machine := &agentHostTestMachine{registry: registry}
	request := &agentv1.SessionAttach{Grant: proto.Clone(session.grant).(*agentv1.SessionGrant), AttachmentSequence: 1}
	_, _, err := computerhost.OpenAgentSession(t.Context(), machine, request)
	if !errors.Is(err, computerhost.ErrAgentSessionAbsent) {
		t.Fatalf("missing explicit absence: %v", err)
	}
	request.Grant.ChannelCredential = "wrong"
	_, _, err = computerhost.OpenAgentSession(t.Context(), machine, request)
	if err == nil || errors.Is(err, computerhost.ErrAgentSessionAbsent) {
		t.Fatalf("unowned absence: %v", err)
	}
}
