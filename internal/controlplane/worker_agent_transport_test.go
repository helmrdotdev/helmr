package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/computerhost"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
)

type workerAgentTestMachine struct{ handle func(net.Conn) }

func (*workerAgentTestMachine) Stream() vm.Stream          { return nil }
func (*workerAgentTestMachine) Wait(context.Context) error { return nil }
func (*workerAgentTestMachine) Close(context.Context) error {
	return errors.New("must not stop Computer")
}
func (m *workerAgentTestMachine) OpenStream(context.Context) (vm.Stream, error) {
	host, guest := net.Pipe()
	go func() { defer guest.Close(); m.handle(guest) }()
	return host, nil
}
func (*workerAgentTestMachine) WithRunningGuestControl(ctx context.Context, _ vm.GuestControlStage, run func(context.Context) error) error {
	return run(ctx)
}

func TestWorkerAgentInvalidArgumentsSettleWithoutPoisoningAttachment(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	identity := &agentv1.SessionIdentity{SessionId: f.Session.String(), ProcessEpoch: 1}
	finished := make(chan struct{})
	machine := &workerAgentTestMachine{handle: func(stream net.Conn) {
		defer close(finished)
		header, _, err := wire.ReadStreamFrameHeader(stream)
		if err != nil || header.Type != wire.StreamTypeAgentSession {
			t.Errorf("header: %v %v", header, err)
			return
		}
		var attach agentv1.SessionAttach
		if err = frameio.ReadProtoFrameBounded(stream, 1<<20, &attach); err != nil {
			t.Error(err)
			return
		}
		if err = frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: identity, AttachmentSequence: 1, Message: &agentv1.GuestSessionMessage_Attached{Attached: &agentv1.SessionAttached{Ready: true}}}); err != nil {
			t.Error(err)
			return
		}
		valid := runtimeEnqueue(f, "valid-after-error").Payload
		nulInput := []byte(`{"tool":"enqueue","arguments":{"sessionId":"` + f.Session.String() + `","input":[{"type":"text","text":"\u0000"}],"idempotencyKey":"nul"}}`)
		invalidCanonical := []byte(`{"tool":"enqueue","arguments":{"sessionId":"` + f.Session.String() + `","input":{"duplicate":1,"duplicate":2},"idempotencyKey":"ambiguous"}}`)
		for index, payload := range [][]byte{[]byte(`{"tool":"enqueue","arguments":{"sessionId":"not-a-uuid","input":1,"idempotencyKey":"bad"}}`), nulInput, invalidCanonical, valid} {
			id := []string{"bad", "nul", "ambiguous", "good"}[index]
			event := &agentv1.ProgramEvent{Identity: identity, Event: &agentv1.ProgramEvent_Operation{Operation: &agentv1.Operation{RequestId: id, Method: agentv1.Operation_METHOD_RUNTIME_MCP, PayloadJson: payload}}}
			if err = frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: identity, AttachmentSequence: 1, EventSequence: uint64(index + 1), OperationAuthorityGeneration: 1, OperationLeaseEpoch: 1, Message: &agentv1.GuestSessionMessage_Event{Event: event}}); err != nil {
				t.Error(err)
				return
			}
			var receipt agentv1.HostSessionMessage
			if err = frameio.ReadProtoFrameBounded(stream, 1<<20, &receipt); err != nil {
				t.Errorf("receipt for %s: %v", id, err)
				return
			}
			result := receipt.GetCommand().GetOperationResult()
			if result.GetRequestId() != id || ((index == 0 || index == 2) && result.GetError().GetCode() != "invalid_arguments") || ((index == 1 || index == 3) && (result.GetError() != nil || len(result.GetValueJson()) == 0)) {
				t.Errorf("unexpected %s receipt: %v", id, result)
				return
			}
			var ack agentv1.HostSessionMessage
			if err = frameio.ReadProtoFrameBounded(stream, 1<<20, &ack); err != nil {
				t.Error(err)
				return
			}
			if ack.GetAcknowledged().GetThroughSequence() != uint64(index+1) {
				t.Errorf("wrong ack: %v", &ack)
				return
			}
		}
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	grant := &agentv1.SessionGrant{Identity: identity, ComputerId: f.Computer.String(), ComputerInstanceId: "instance", WriterGeneration: 1, WorkerHostId: f.Worker.String(), ComputerLeaseEpoch: 1, AuthorityGeneration: 1, ExpiresAtUnixNano: time.Now().Add(time.Hour).UnixNano(), ChannelCredential: "private"}
	connection, _, err := computerhost.OpenAgentSession(ctx, machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = computerhost.ServeAgentSession(ctx, connection, f.Environment.String(), client, func(context.Context, *agentv1.GuestSessionMessage) error {
		return errors.New("unexpected observer event")
	})
	<-finished
	var count int
	if err = f.Pool.QueryRow(t.Context(), "SELECT count(*) FROM turns").Scan(&count); err != nil || count != 2 {
		t.Fatalf("valid follow-up turns=%d err=%v", count, err)
	}
	var stored []byte
	if err = f.Pool.QueryRow(t.Context(), "SELECT input FROM turns WHERE retry_key='nul'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var value []struct {
		Text string `json:"text"`
	}
	if err = json.Unmarshal(stored, &value); err != nil || len(value) != 1 || value[0].Text != "\x00" {
		t.Fatalf("NUL input round trip: %+v %v", value, err)
	}
}

func TestWorkerAgentServiceRenewsCurrentGenerationWithoutSetup(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	dbtest.MustExec(t, t.Context(), f.Pool, "UPDATE sessions SET authority_generation=2")
	identity := &agentv1.SessionIdentity{SessionId: f.Session.String(), ProcessEpoch: 1}
	observed := make(chan *agentv1.SessionGrant, 1)
	machine := &workerAgentTestMachine{handle: func(stream net.Conn) {
		if _, _, err := wire.ReadStreamFrameHeader(stream); err != nil {
			t.Error(err)
			return
		}
		var attach agentv1.SessionAttach
		if err := frameio.ReadProtoFrameBounded(stream, 1<<20, &attach); err != nil {
			t.Error(err)
			return
		}
		if attach.GetStart() != nil {
			t.Error("renewal re-ran setup")
			return
		}
		if err := frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: identity, AttachmentSequence: 1, Message: &agentv1.GuestSessionMessage_Attached{Attached: &agentv1.SessionAttached{Ready: true}}}); err != nil {
			t.Error(err)
			return
		}
		var message agentv1.HostSessionMessage
		if err := frameio.ReadProtoFrameBounded(stream, 1<<20, &message); err != nil {
			t.Error(err)
			return
		}
		observed <- message.GetRenew()
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	grant := &agentv1.SessionGrant{Identity: identity, ComputerId: f.Computer.String(), ComputerInstanceId: "instance", WriterGeneration: 1, WorkerHostId: f.Worker.String(), ComputerLeaseEpoch: 1, AuthorityGeneration: 1, ExpiresAtUnixNano: time.Now().Add(100 * time.Millisecond).UnixNano(), ChannelCredential: "private"}
	connection, _, err := computerhost.OpenAgentSession(ctx, machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = computerhost.ServeAgentSession(ctx, connection, f.Environment.String(), client, func(context.Context, *agentv1.GuestSessionMessage) error {
		return errors.New("unexpected observer event")
	})
	select {
	case next := <-observed:
		if next.GetAuthorityGeneration() != 2 || next.GetComputerLeaseEpoch() != 1 || next.GetChannelCredential() != grant.ChannelCredential || next.GetIdentity().GetProcessEpoch() != 1 || next.GetExpiresAtUnixNano() <= time.Now().UnixNano() {
			t.Fatalf("renewal: %v", next)
		}
	default:
		t.Fatal("no renewed grant")
	}
	// The typed terminal signal is distinct from temporary CP/network failure.
	dbtest.MustExec(t, t.Context(), f.Pool, "UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'")
	_, err = client.RenewAgentAuthority(t.Context(), runtimeTestSession(f))
	var denied interface{ SessionAuthorityRejected() bool }
	if !errors.As(err, &denied) || !denied.SessionAuthorityRejected() {
		t.Fatalf("expiry is not a terminal owner reconciliation signal: %v", err)
	}
}

// Exercise both authentication lifetimes through the real HTTP server and
// PostgreSQL while retaining one guest attachment. Native processes and VM
// suspension are qualified separately; the wire peer here is synthetic.
func TestWorkerAgentHotCredentialRefreshPreservesAttachment(t *testing.T) {
	f := agenttest.New(t)
	handler := newPostgresServer(t, f.Pool, func(cfg *ServerConfig) {
		cfg.WorkerHostCredentialTTL = 2 * time.Second
	})
	var exchanges atomic.Int64
	firstCredential := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/instance/credential" {
			exchanges.Add(1)
		}
		if r.URL.Path == "/worker/v1/sessions/authority" {
			select {
			case firstCredential <- r.Header.Get("Authorization"):
			default:
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	identity := &agentv1.SessionIdentity{SessionId: f.Session.String(), ProcessEpoch: 1}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	machine := &workerAgentTestMachine{handle: func(stream net.Conn) {
		finished <- func() error {
			if _, _, err := wire.ReadStreamFrameHeader(stream); err != nil {
				return err
			}
			var attach agentv1.SessionAttach
			if err := frameio.ReadProtoFrameBounded(stream, 1<<20, &attach); err != nil {
				return err
			}
			if attach.GetStart() != nil {
				return errors.New("retained attachment started setup")
			}
			if err := frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: identity, AttachmentSequence: 1, Message: &agentv1.GuestSessionMessage_Attached{Attached: &agentv1.SessionAttached{Ready: true}}}); err != nil {
				return err
			}
			var renewed agentv1.HostSessionMessage
			if err := frameio.ReadProtoFrameBounded(stream, 1<<20, &renewed); err != nil {
				return err
			}
			next := renewed.GetRenew()
			if next.GetChannelCredential() != attach.GetGrant().GetChannelCredential() || next.GetIdentity().GetProcessEpoch() != 1 || next.GetExpiresAtUnixNano() <= time.Now().UnixNano() {
				return errors.New("renewal changed local identity or failed to extend authority")
			}
			for index, key := range []string{"before-expiry", "after-expiry"} {
				if index == 1 {
					// Let the server-issued JWT actually expire, not just enter the
					// client's proactive refresh window.
					timer := time.NewTimer(3 * time.Second)
					select {
					case <-timer.C:
					case <-ctx.Done():
						timer.Stop()
						return ctx.Err()
					}
				}
				event := &agentv1.ProgramEvent{Identity: identity, Event: &agentv1.ProgramEvent_Operation{Operation: &agentv1.Operation{RequestId: key, Method: agentv1.Operation_METHOD_RUNTIME_MCP, PayloadJson: runtimeEnqueue(f, key).Payload}}}
				if err := frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: identity, AttachmentSequence: 1, EventSequence: uint64(index + 1), OperationAuthorityGeneration: next.AuthorityGeneration, OperationLeaseEpoch: next.ComputerLeaseEpoch, Message: &agentv1.GuestSessionMessage_Event{Event: event}}); err != nil {
					return err
				}
				var reply, ack agentv1.HostSessionMessage
				if err := frameio.ReadProtoFrameBounded(stream, 1<<20, &reply); err != nil {
					return err
				}
				if reply.GetCommand().GetOperationResult().GetRequestId() != key || len(reply.GetCommand().GetOperationResult().GetValueJson()) == 0 {
					return errors.New("authenticated MCP request did not settle")
				}
				if err := frameio.ReadProtoFrameBounded(stream, 1<<20, &ack); err != nil {
					return err
				}
				if ack.GetAcknowledged().GetThroughSequence() != uint64(index+1) {
					return errors.New("operation acknowledgment changed")
				}
			}
			return nil
		}()
	}}
	grant := &agentv1.SessionGrant{Identity: identity, ComputerId: f.Computer.String(), ComputerInstanceId: "instance", WriterGeneration: 1, WorkerHostId: f.Worker.String(), ComputerLeaseEpoch: 1, AuthorityGeneration: 1, ExpiresAtUnixNano: time.Now().Add(100 * time.Millisecond).UnixNano(), ChannelCredential: "unchanged-local-credential"}
	connection, _, err := computerhost.OpenAgentSession(ctx, machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = computerhost.ServeAgentSession(ctx, connection, f.Environment.String(), client, func(context.Context, *agentv1.GuestSessionMessage) error {
		return errors.New("unexpected non-operation event")
	})
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	// Bypass automatic refresh to verify the original credential is no longer
	// accepted by the server. The rejected mutation must not create a Turn.
	stale, err := json.Marshal(runtimeEnqueue(f, "stale-credential"))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/sessions/operations", bytes.NewReader(stale))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", <-firstCredential)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expired credential status=%d", response.Code)
	}
	if exchanges.Load() < 2 {
		t.Fatalf("host credential was not exchanged again: %d", exchanges.Load())
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), "SELECT count(*) FROM turns").Scan(&count); err != nil || count != 2 {
		t.Fatalf("accepted turns=%d err=%v", count, err)
	}
}
