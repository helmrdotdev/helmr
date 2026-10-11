package computerhost

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

type agentRuntimeTestClient struct {
	operation func(context.Context, workerapi.AgentOperationRequest) (workerapi.AgentOperationResponse, error)
}

func (client agentRuntimeTestClient) AgentOperation(ctx context.Context, r workerapi.AgentOperationRequest) (workerapi.AgentOperationResponse, error) {
	return client.operation(ctx, r)
}
func (client agentRuntimeTestClient) RenewAgentAuthority(ctx context.Context, r workerapi.RuntimeSession) (workerapi.AgentAuthorityResponse, error) {
	return workerapi.AgentAuthorityResponse{}, errors.New("renewal unexpected before half-life")
}

func TestAgentServiceForwardsOriginalIdentityAndReceipt(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "value", true: "error"}[failed], func(t *testing.T) {
			grant := sessionTransportGrant()
			observed := make(chan *agentv1.OperationResult, 1)
			machine := sessionTransportMachine(t, func(stream net.Conn, attach *agentv1.SessionAttach) {
				event := &agentv1.GuestSessionMessage{Identity: grant.Identity, AttachmentSequence: attach.AttachmentSequence, EventSequence: 8, OperationAuthorityGeneration: 2, OperationLeaseEpoch: 2, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: grant.Identity, Event: &agentv1.ProgramEvent_Operation{Operation: &agentv1.Operation{RequestId: "original", Method: agentv1.Operation_METHOD_RUNTIME_MCP, PayloadJson: []byte(`{"tool":"enqueue","arguments":{"idempotencyKey":"stable"}}`)}}}}}
				if err := frameio.WriteProtoFrame(stream, event); err != nil {
					t.Error(err)
					return
				}
				var result agentv1.HostSessionMessage
				if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &result); err != nil {
					t.Error(err)
					return
				}
				observed <- result.GetCommand().GetOperationResult()
				var ack agentv1.HostSessionMessage
				if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &ack); err != nil {
					t.Error(err)
					return
				}
				if ack.GetAcknowledged().GetThroughSequence() != 8 {
					t.Errorf("wrong acknowledgment: %v", &ack)
				}
			})
			connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 4})
			if err != nil {
				t.Fatal(err)
			}
			client := agentRuntimeTestClient{operation: func(_ context.Context, r workerapi.AgentOperationRequest) (workerapi.AgentOperationResponse, error) {
				if r.AuthorityGeneration != 2 || r.Session.ComputerLeaseEpoch != 2 || r.Session.EnvironmentID != "environment" || r.Session.SessionID != "session" || r.Session.ProcessEpoch != 1 || r.TurnID != "" || r.RequestID != "original" {
					t.Errorf("identity changed: %+v", r)
				}
				if failed {
					return workerapi.AgentOperationResponse{RequestID: r.RequestID, Error: &workerapi.AgentOperationError{Code: "authority_changed", Message: "denied"}}, nil
				}
				return workerapi.AgentOperationResponse{RequestID: r.RequestID, Value: []byte(`{"turnId":"accepted"}`)}, nil
			}}
			_ = ServeAgentSession(t.Context(), connection, "environment", client, func(context.Context, *agentv1.GuestSessionMessage) error {
				return errors.New("unexpected non-operation event")
			})
			select {
			case got := <-observed:
				if got.GetRequestId() != "original" || (failed && got.GetError().GetCode() != "authority_changed") || (!failed && string(got.GetValueJson()) != `{"turnId":"accepted"}`) {
					t.Fatalf("receipt: %v", got)
				}
			default:
				t.Fatal("missing operation receipt")
			}
		})
	}
}

func TestAgentServiceUncertainOperationClosesOnlyAttachment(t *testing.T) {
	grant := sessionTransportGrant()
	machine := sessionTransportMachine(t, func(stream net.Conn, attach *agentv1.SessionAttach) {
		event := &agentv1.GuestSessionMessage{Identity: grant.Identity, AttachmentSequence: attach.AttachmentSequence, EventSequence: 1, OperationAuthorityGeneration: 3, OperationLeaseEpoch: 2, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: grant.Identity, Event: &agentv1.ProgramEvent_Operation{Operation: &agentv1.Operation{RequestId: "uncertain", Method: agentv1.Operation_METHOD_ENQUEUE, TurnId: proto.String("turn"), PayloadJson: []byte(`{}`)}}}}}
		if err := frameio.WriteProtoFrame(stream, event); err != nil {
			t.Error(err)
			return
		}
		var reply agentv1.HostSessionMessage
		if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &reply); err == nil {
			t.Errorf("uncertain operation was acknowledged or completed: %v", &reply)
		}
	})
	connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	uncertain := errors.New("upstream reply lost")
	client := agentRuntimeTestClient{operation: func(context.Context, workerapi.AgentOperationRequest) (workerapi.AgentOperationResponse, error) {
		return workerapi.AgentOperationResponse{}, uncertain
	}}
	err = ServeAgentSession(t.Context(), connection, "environment", client, func(context.Context, *agentv1.GuestSessionMessage) error { return nil })
	if !errors.Is(err, uncertain) {
		t.Fatalf("uncertainty lost: %v", err)
	}
}

func TestAgentServiceSettlesInvalidAuthoredEnvelope(t *testing.T) {
	for _, invalid := range []string{"json", "id"} {
		t.Run(invalid, func(t *testing.T) {
			grant := sessionTransportGrant()
			id, payload := "invalid", []byte(`{}`)
			if invalid == "json" {
				payload = []byte(`{bad`)
			} else {
				id = strings.Repeat("x", 513)
			}
			observed := make(chan *agentv1.OperationResult, 1)
			machine := sessionTransportMachine(t, func(stream net.Conn, attach *agentv1.SessionAttach) {
				event := &agentv1.GuestSessionMessage{Identity: grant.Identity, AttachmentSequence: attach.AttachmentSequence, EventSequence: 1, OperationAuthorityGeneration: 3, OperationLeaseEpoch: 2, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: grant.Identity, Event: &agentv1.ProgramEvent_Operation{Operation: &agentv1.Operation{RequestId: id, Method: agentv1.Operation_METHOD_ENQUEUE, TurnId: proto.String("turn"), PayloadJson: payload}}}}}
				if err := frameio.WriteProtoFrame(stream, event); err != nil {
					t.Error(err)
					return
				}
				var reply agentv1.HostSessionMessage
				if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &reply); err != nil {
					t.Error(err)
					return
				}
				observed <- reply.GetCommand().GetOperationResult()
				var ack agentv1.HostSessionMessage
				if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &ack); err != nil {
					t.Error(err)
					return
				}
				if ack.GetAcknowledged().GetThroughSequence() != 1 {
					t.Error("rejection not acknowledged")
				}
			})
			connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 1})
			if err != nil {
				t.Fatal(err)
			}
			client := agentRuntimeTestClient{operation: func(context.Context, workerapi.AgentOperationRequest) (workerapi.AgentOperationResponse, error) {
				t.Error("invalid envelope reached CP encoder")
				return workerapi.AgentOperationResponse{}, errors.New("unexpected")
			}}
			_ = ServeAgentSession(t.Context(), connection, "environment", client, func(context.Context, *agentv1.GuestSessionMessage) error { return nil })
			select {
			case result := <-observed:
				if result.GetRequestId() != id || result.GetError().GetCode() != "invalid_arguments" {
					t.Fatalf("rejection: %v", result)
				}
			default:
				t.Fatal("invalid operation did not settle")
			}
		})
	}
}

func TestAgentServiceBlockedDiagnosticsDoNotBlockOperations(t *testing.T) {
	grant := sessionTransportGrant()
	releaseLog := make(chan struct{})
	logEntered := make(chan struct{})
	machine := sessionTransportMachine(t, func(stream net.Conn, attach *agentv1.SessionAttach) {
		write := func(message *agentv1.GuestSessionMessage) bool {
			message.Identity, message.AttachmentSequence = grant.Identity, attach.AttachmentSequence
			if err := frameio.WriteProtoFrame(stream, message); err != nil {
				t.Error(err)
				return false
			}
			return true
		}
		read := func() *agentv1.HostSessionMessage {
			message := new(agentv1.HostSessionMessage)
			if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, message); err != nil {
				t.Error(err)
			}
			return message
		}
		if !write(&agentv1.GuestSessionMessage{Message: &agentv1.GuestSessionMessage_Log{Log: &agentv1.SessionLog{Stream: agentv1.SessionLog_STREAM_STDOUT, Kind: agentv1.SessionLog_KIND_DATA, Sequence: 7, ThroughSequence: 7, ObservedAtUnixNano: 1, Data: []byte{255, 0}}}}) {
			return
		}
		<-logEntered
		if !write(&agentv1.GuestSessionMessage{EventSequence: 1, OperationAuthorityGeneration: 3, OperationLeaseEpoch: 2, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: grant.Identity, Event: &agentv1.ProgramEvent_Operation{Operation: &agentv1.Operation{RequestId: "during-log-outage", Method: agentv1.Operation_METHOD_ENQUEUE, PayloadJson: []byte(`{}`)}}}}}) {
			return
		}
		if result := read(); result.GetCommand().GetOperationResult().GetRequestId() != "during-log-outage" {
			t.Errorf("operation blocked: %v", result)
			return
		}
		if ack := read(); ack.GetAcknowledged().GetThroughSequence() != 1 {
			t.Errorf("control ack changed: %v", ack)
			return
		}
		close(releaseLog)
		if ack := read(); ack.GetLogAcknowledged().GetThroughSequence() != 7 || ack.GetAcknowledged() != nil {
			t.Errorf("durable log receipt: %v", ack)
			return
		}
		if !write(&agentv1.GuestSessionMessage{EventSequence: 2, Message: &agentv1.GuestSessionMessage_Stopped{Stopped: &agentv1.SessionStopped{}}}) {
			return
		}
		if ack := read(); ack.GetAcknowledged().GetThroughSequence() != 2 {
			t.Errorf("stopped ack: %v", ack)
		}
	})
	connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	client := agentRuntimeTestClient{operation: func(_ context.Context, request workerapi.AgentOperationRequest) (workerapi.AgentOperationResponse, error) {
		return workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: []byte(`null`)}, nil
	}}
	err = ServeAgentSession(t.Context(), connection, "environment", client, func(ctx context.Context, event *agentv1.GuestSessionMessage) error {
		if event.GetLog() != nil {
			close(logEntered)
			select {
			case <-releaseLog:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAgentServiceTerminalDiagnosticDrain(t *testing.T) {
	for _, expires := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry_then_EOF", true: "outage_until_grant_expiry"}[expires], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				grant := sessionTransportGrant()
				lifetime := 20 * time.Second
				if expires {
					lifetime = 3 * time.Second
				}
				grant.ExpiresAtUnixNano = started.Add(lifetime).UnixNano()
				var attempts atomic.Int32
				machine := sessionTransportMachine(t, func(stream net.Conn, attach *agentv1.SessionAttach) {
					send := func(message *agentv1.GuestSessionMessage) bool {
						message.Identity, message.AttachmentSequence = grant.Identity, attach.AttachmentSequence
						if err := frameio.WriteProtoFrame(stream, message); err != nil {
							t.Error(err)
							return false
						}
						return true
					}
					read := func() *agentv1.HostSessionMessage {
						result := new(agentv1.HostSessionMessage)
						if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, result); err != nil {
							t.Error(err)
						}
						return result
					}
					if !send(&agentv1.GuestSessionMessage{Message: &agentv1.GuestSessionMessage_Log{Log: &agentv1.SessionLog{Stream: agentv1.SessionLog_STREAM_STDOUT, Kind: agentv1.SessionLog_KIND_DATA, Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte("retained")}}}) {
						return
					}
					if !send(&agentv1.GuestSessionMessage{EventSequence: 1, Message: &agentv1.GuestSessionMessage_Stopped{Stopped: &agentv1.SessionStopped{}}}) {
						return
					}
					if ack := read(); ack.GetAcknowledged().GetThroughSequence() != 1 {
						t.Errorf("stop waited for diagnostics: %v", ack)
						return
					}
					if !expires {
						if ack := read(); ack.GetLogAcknowledged().GetThroughSequence() != 1 {
							t.Errorf("retry receipt %v", ack)
							return
						}
						for _, streamID := range []agentv1.SessionLog_Stream{agentv1.SessionLog_STREAM_STDOUT, agentv1.SessionLog_STREAM_STDERR} {
							seq := int64(1)
							if streamID == agentv1.SessionLog_STREAM_STDOUT {
								seq = 2
							}
							if !send(&agentv1.GuestSessionMessage{Message: &agentv1.GuestSessionMessage_Log{Log: &agentv1.SessionLog{Stream: streamID, Kind: agentv1.SessionLog_KIND_END, Sequence: seq, ThroughSequence: seq, ObservedAtUnixNano: 1, Complete: true}}}) {
								return
							}
							if ack := read(); ack.GetLogAcknowledged().GetStream() != streamID || ack.GetLogAcknowledged().GetThroughSequence() != seq {
								t.Errorf("EOF receipt %v", ack)
								return
							}
						}
					}
					var after agentv1.HostSessionMessage
					if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &after); err == nil {
						t.Errorf("unexpected receipt after terminal drain: %v", &after)
					}
				})
				connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 1})
				if err != nil {
					t.Fatal(err)
				}
				err = ServeAgentSession(t.Context(), connection, "environment", agentRuntimeTestClient{}, func(_ context.Context, event *agentv1.GuestSessionMessage) error {
					if event.GetLog().GetKind() == agentv1.SessionLog_KIND_DATA {
						count := attempts.Add(1)
						if expires || count == 1 {
							return errors.New("ingestion unavailable")
						}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if expires {
					if time.Since(started) != lifetime {
						t.Fatalf("unbounded or premature drain: %s", time.Since(started))
					}
				} else if time.Since(started) != time.Second || attempts.Load() != 2 {
					t.Fatalf("retry/EOF drain: duration=%s attempts=%d", time.Since(started), attempts.Load())
				}
			})
		})
	}
}

func TestAgentServiceDiagnosticDispositionPreservesControl(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired_head", true: "definite_rejection"}[rejected], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				grant := sessionTransportGrant()
				machine := sessionTransportMachine(t, func(stream net.Conn, attach *agentv1.SessionAttach) {
					write := func(message *agentv1.GuestSessionMessage) {
						message.Identity = grant.Identity
						message.AttachmentSequence = attach.AttachmentSequence
						if err := frameio.WriteProtoFrame(stream, message); err != nil {
							t.Error(err)
						}
					}
					read := func() *agentv1.HostSessionMessage {
						message := new(agentv1.HostSessionMessage)
						if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, message); err != nil {
							t.Error(err)
						}
						return message
					}
					write(&agentv1.GuestSessionMessage{Message: &agentv1.GuestSessionMessage_Log{Log: &agentv1.SessionLog{Stream: agentv1.SessionLog_STREAM_STDOUT, Kind: agentv1.SessionLog_KIND_DATA, Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte("old")}}})
					if !rejected {
						ack := read().GetLogAcknowledged()
						if ack.GetThroughSequence() != 1 || !ack.GetExpired() {
							t.Errorf("expiry was claimed as acceptance: %v", ack)
						}
						write(&agentv1.GuestSessionMessage{Message: &agentv1.GuestSessionMessage_Log{Log: &agentv1.SessionLog{Stream: agentv1.SessionLog_STREAM_STDOUT, Kind: agentv1.SessionLog_KIND_END, Sequence: 2, ThroughSequence: 2, ObservedAtUnixNano: 2, Complete: true}}})
						ack = read().GetLogAcknowledged()
						if ack.GetThroughSequence() != 2 || ack.GetExpired() {
							t.Errorf("new head did not progress: %v", ack)
						}
					}
					write(&agentv1.GuestSessionMessage{Message: &agentv1.GuestSessionMessage_Log{Log: &agentv1.SessionLog{Stream: agentv1.SessionLog_STREAM_STDERR, Kind: agentv1.SessionLog_KIND_END, Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Complete: true}}})
					if ack := read().GetLogAcknowledged(); ack.GetStream() != agentv1.SessionLog_STREAM_STDERR || ack.GetExpired() {
						t.Errorf("other stream blocked: %v", ack)
					}
					write(&agentv1.GuestSessionMessage{EventSequence: 1, Message: &agentv1.GuestSessionMessage_Stopped{Stopped: &agentv1.SessionStopped{}}})
					if ack := read().GetAcknowledged(); ack.GetThroughSequence() != 1 {
						t.Errorf("stop receipt blocked: %v", ack)
					}
				})
				connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 1})
				if err != nil {
					t.Fatal(err)
				}
				var attempts atomic.Int32
				client := &terminalControlClient{logs: func(_ context.Context, request workerapi.SessionLogRequest) (workerapi.DiagnosticLogReceipt, error) {
					if request.Stream == "stdout" && request.Sequence == 1 {
						attempts.Add(1)
						if rejected {
							return workerapi.DiagnosticLogReceipt{}, &httpclient.Error{StatusCode: 409}
						}
						return workerapi.DiagnosticLogReceipt{ThroughSequence: 1, Expired: true}, nil
					}
					accepted := time.Now()
					return workerapi.DiagnosticLogReceipt{ThroughSequence: request.ThroughSequence, AcceptedAt: accepted, ExpiresAt: accepted.Add(90 * 24 * time.Hour)}, nil
				}}
				err = ServeAgentSession(t.Context(), connection, "environment", client, func(ctx context.Context, event *agentv1.GuestSessionMessage) error {
					if log := event.GetLog(); log != nil {
						return connection.appendSessionLog(ctx, "environment", client, log)
					}
					return nil
				})
				if err != nil || attempts.Load() != 1 || time.Since(started) != 0 {
					t.Fatalf("disposition retried or delayed lifecycle: attempts=%d elapsed=%s err=%v", attempts.Load(), time.Since(started), err)
				}
			})
		})
	}
}
