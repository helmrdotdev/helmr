package computerhost

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type retryOperationClient struct {
	agentRuntimeTestClient
	renew func(context.Context) (workerapi.AgentAuthorityResponse, error)
}

func (c retryOperationClient) RenewAgentAuthority(ctx context.Context, _ workerapi.RuntimeSession) (workerapi.AgentAuthorityResponse, error) {
	return c.renew(ctx)
}

func retryOperationEvent(grant *agentv1.SessionGrant, attachment uint64) *agentv1.GuestSessionMessage {
	return &agentv1.GuestSessionMessage{Identity: grant.Identity, AttachmentSequence: attachment, EventSequence: 1, OperationAuthorityGeneration: 3, OperationLeaseEpoch: 2, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: grant.Identity, Event: &agentv1.ProgramEvent_Operation{Operation: &agentv1.Operation{RequestId: "stable-finalize", Method: agentv1.Operation_METHOD_FINALIZE, PayloadJson: []byte(`{"result":7}`), TurnId: ptrRetryTurn()}}}}}
}
func ptrRetryTurn() *string { turn := "turn"; return &turn }

func TestAgentServicePendingFinalizeKeepsAttachmentAndCallbacks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		grant := sessionTransportGrant()
		grant.ExpiresAtUnixNano = time.Now().Add(time.Second).UnixNano()
		first, renewed, callback := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var attempts, attachments atomic.Int32
		machine := sessionTransportMachine(t, func(stream net.Conn, attach *agentv1.SessionAttach) {
			attachments.Add(1)
			if err := frameio.WriteProtoFrame(stream, retryOperationEvent(grant, attach.AttachmentSequence)); err != nil {
				t.Error(err)
				return
			}
			<-first
			event := &agentv1.GuestSessionMessage{Identity: grant.Identity, AttachmentSequence: attach.AttachmentSequence, EventSequence: 2, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: grant.Identity, Event: &agentv1.ProgramEvent_DeliveryResult{DeliveryResult: &agentv1.DeliveryResult{DeliveryId: "message:callback", Outcome: &agentv1.DeliveryResult_ValueJson{ValueJson: []byte(`null`)}}}}}}
			if err := frameio.WriteProtoFrame(stream, event); err != nil {
				t.Error(err)
				return
			}
			seenResult, seenOperationAck, seenCallbackAck, seenRenew := false, false, false, false
			for !(seenResult && seenOperationAck && seenCallbackAck && seenRenew) {
				var message agentv1.HostSessionMessage
				if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &message); err != nil {
					t.Error(err)
					return
				}
				switch {
				case message.GetRenew() != nil:
					seenRenew = true
				case message.GetCommand().GetOperationResult() != nil:
					seenResult = true
					if message.GetCommand().GetOperationResult().GetRequestId() != "stable-finalize" {
						t.Error("request identity changed")
					}
				case message.GetAcknowledged().GetThroughSequence() == 1:
					seenOperationAck = true
				case message.GetAcknowledged().GetThroughSequence() == 2:
					seenCallbackAck = true
				default:
					t.Errorf("unexpected receipt: %v", &message)
					return
				}
			}
		})
		connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 1})
		if err != nil {
			t.Fatal(err)
		}
		var original workerapi.AgentOperationRequest
		client := retryOperationClient{operation: func(_ context.Context, request workerapi.AgentOperationRequest) (workerapi.AgentOperationResponse, error) {
			if attempts.Add(1) == 1 {
				original = request
				close(first)
			} else if !reflect.DeepEqual(original, request) {
				t.Error("pending operation changed across retry")
			}
			select {
			case <-callback:
				return workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: []byte(`7`)}, nil
			default:
				return workerapi.AgentOperationResponse{}, &httpclient.Error{StatusCode: 503, Code: workerapi.TurnReceiptsPending}
			}
		}, renew: func(context.Context) (workerapi.AgentAuthorityResponse, error) {
			close(renewed)
			return workerapi.AgentAuthorityResponse{AuthorityGeneration: 3, ExpiresAt: time.Now().Add(time.Hour)}, nil
		}}
		_ = serveAgentSessionForTest(t.Context(), connection, "environment", client, func(_ context.Context, message *agentv1.GuestSessionMessage) error {
			if message.GetEvent().GetDeliveryResult().GetDeliveryId() != "message:callback" {
				return errors.New("unexpected observation")
			}
			<-renewed
			close(callback)
			return nil
		})
		if attachments.Load() != 1 || attempts.Load() < 2 {
			t.Fatalf("attachments=%d attempts=%d", attachments.Load(), attempts.Load())
		}
	})
}

func TestAgentServiceOperationRetryCancellationJoins(t *testing.T) {
	for _, inflight := range []bool{false, true} {
		t.Run(map[bool]string{false: "backoff", true: "http_request"}[inflight], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				grant := sessionTransportGrant()
				machine := sessionTransportMachine(t, func(stream net.Conn, attach *agentv1.SessionAttach) {
					if err := frameio.WriteProtoFrame(stream, retryOperationEvent(grant, attach.AttachmentSequence)); err != nil {
						t.Error(err)
						return
					}
					var message agentv1.HostSessionMessage
					if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &message); err == nil {
						t.Error("canceled retry sent receipt")
					}
				})
				connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 1})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancelCause(t.Context())
				entered, joined := make(chan struct{}), make(chan struct{})
				var attempts atomic.Int32
				client := agentRuntimeTestClient{operation: func(ctx context.Context, _ workerapi.AgentOperationRequest) (workerapi.AgentOperationResponse, error) {
					if attempts.Add(1) == 1 {
						if !inflight {
							close(entered)
						}
						return workerapi.AgentOperationResponse{}, &httpclient.Error{StatusCode: 503, Code: workerapi.TurnReceiptsPending}
					}
					close(entered)
					defer close(joined)
					<-ctx.Done()
					return workerapi.AgentOperationResponse{}, ctx.Err()
				}}
				done := make(chan error, 1)
				go func() {
					done <- serveAgentSessionForTest(ctx, connection, "environment", client, func(context.Context, *agentv1.GuestSessionMessage) error { return nil })
				}()
				<-entered
				cause := errors.New("lease ended")
				cancel(cause)
				if err := <-done; !errors.Is(err, cause) {
					t.Fatal(err)
				}
				if inflight {
					select {
					case <-joined:
					default:
						t.Fatal("retry HTTP request was not joined")
					}
				} else if attempts.Load() != 1 {
					t.Fatal("backoff ignored cancellation")
				}
			})
		})
	}
}

func TestAgentOperationRequestRetryClassification(t *testing.T) {
	for _, test := range []struct {
		method workerapi.AgentOperationMethod
		err    error
		retry  bool
	}{
		{workerapi.AgentMethodFinalize, &httpclient.Error{StatusCode: 503, Code: workerapi.TurnReceiptsPending}, true},
		{workerapi.AgentMethodFinalize, &httpclient.Error{StatusCode: 503}, false},
		{workerapi.AgentMethodRuntimeMCP, &httpclient.Error{StatusCode: 503, Code: workerapi.TurnReceiptsPending}, false},
		{workerapi.AgentMethodFinalize, io.ErrUnexpectedEOF, false},
		{workerapi.AgentMethodFinalize, &httpclient.Error{StatusCode: 403, Code: workerapi.TurnReceiptsPending}, false},
		{workerapi.AgentMethodFinalize, errors.New("invalid receipt"), false},
	} {
		if got := agentOperationRequestRetryable(test.method, test.err); got != test.retry {
			t.Fatalf("%s %v retry=%v", test.method, test.err, got)
		}
	}
}
