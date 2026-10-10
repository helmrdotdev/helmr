package computerhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestPreparationLogRetriesPreserveHeadAndAcknowledgeExactReceipt(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "expired"}[expired], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				owner, client, backend := preparationOwnerFixture(t)
				owner.machine = backend.machine
				owner.executor = workerapi.PreparationExecutor{Identity: owner.identity, ChannelCredential: bytes.Repeat([]byte{11}, 32)}
				owner.expires.Store(time.Now().Add(time.Minute).UnixNano())
				observed := time.Now().UnixNano()
				var uploads, acks, ends atomic.Int64
				backend.machine.handle = func(stream net.Conn) {
					if _, _, err := wire.ReadStreamFrameHeader(stream); err != nil {
						t.Error(err)
						return
					}
					var request computerv0.PreparationControlRequest
					if err := frameio.ReadProtoFrame(stream, &request); err != nil {
						t.Error(err)
						return
					}
					response := &computerv0.PreparationControlResponse{State: "succeeded"}
					switch request.AcknowledgedThrough {
					case 0:
						response.Log = &computerv0.PreparationLog{Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: observed, Data: []byte{0, 255, 128}}
					case 1:
						if acks.Add(1) == 1 {
							return
						} // The guest committed this ACK; its reply disappears.
						response.Log = &computerv0.PreparationLog{Kind: "end", Sequence: 2, ThroughSequence: 2, ObservedAtUnixNano: observed, Complete: true}
					case 2:
						ends.Add(1)
					default:
						t.Errorf("skipped acknowledgment %d", request.AcknowledgedThrough)
					}
					_ = frameio.WriteProtoFrame(stream, response)
				}
				client.appendLog = func(_ context.Context, r workerapi.PreparationLogRequest) (workerapi.DiagnosticLogReceipt, error) {
					if r.Executor.Identity != owner.identity || !bytes.Equal(r.Executor.ChannelCredential, owner.executor.ChannelCredential) || r.Stream != "stdout" || r.ObservedAtUnixNano != observed {
						t.Errorf("changed identity: %v", r)
					}
					if r.Kind == "data" {
						if !bytes.Equal(r.Data, []byte{0, 255, 128}) || r.Sequence != 1 || r.ThroughSequence != 1 {
							t.Errorf("changed bytes: %v", r)
						}
						if uploads.Add(1) == 1 {
							return workerapi.DiagnosticLogReceipt{}, errors.New("committed receipt lost")
						}
					}
					if expired {
						return workerapi.DiagnosticLogReceipt{Expired: true, ThroughSequence: r.ThroughSequence}, nil
					}
					now := time.Now()
					return workerapi.DiagnosticLogReceipt{ThroughSequence: r.ThroughSequence, AcceptedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour)}, nil
				}
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				owner.deliverPreparationLogs(ctx, "stdout")
				if ctx.Err() != nil || uploads.Load() != 2 || acks.Load() != 2 || ends.Load() != 1 {
					t.Fatalf("uploads=%d ACKs=%d ends=%d err=%v", uploads.Load(), acks.Load(), ends.Load(), ctx.Err())
				}
			})
		})
	}
}

// Exercise received rejection headers through the actual worker HTTP client,
// including a body read failure. Only the rejected stream stops; no guest ACK
// may attest rejected bytes, and the other pipe and publication must finish.
func TestPreparationLogHTTPRejectionIsolatedFromOtherStreamAndPublication(t *testing.T) {
	for _, status := range []int{409, 410} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				owner, client, backend := preparationOwnerFixture(t)
				var stdoutCalls, stderrCalls, stdoutACK, stderrACK atomic.Int64
				transport := preparationLogRoundTrip(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/worker/v1/instance/credential" {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"credential":"fixture","expires_in_seconds":3600,"worker_epoch":7}`)), Header: make(http.Header)}, nil
					}
					var request workerapi.PreparationLogRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						return nil, err
					}
					if request.Stream == "stdout" {
						stdoutCalls.Add(1)
						return &http.Response{StatusCode: status, Status: strconv.Itoa(status), Body: io.NopCloser(preparationLogBrokenBody{}), Header: make(http.Header)}, nil
					}
					stderrCalls.Add(1)
					now := time.Now()
					body, _ := json.Marshal(workerapi.DiagnosticLogReceipt{ThroughSequence: request.ThroughSequence, AcceptedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour)})
					return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
				})
				actual, err := workerclient.New("http://127.0.0.1", workerclient.WithAuth("host", "secret"), workerclient.WithService("fixture"), workerclient.WithHTTPClient(&http.Client{Transport: transport}))
				if err != nil {
					t.Fatal(err)
				}
				client.appendLog = actual.AppendPreparationLog
				fallback := backend.machine.handle
				observed := time.Now().UnixNano()
				backend.machine.handle = func(stream net.Conn) {
					raw := new(bytes.Buffer)
					header, _, err := wire.ReadStreamFrameHeader(io.TeeReader(stream, raw))
					if err != nil {
						t.Error(err)
						return
					}
					if header.Type != wire.StreamTypePreparationControl {
						fallback(&preparationReplayConn{Conn: stream, reader: io.MultiReader(bytes.NewReader(raw.Bytes()), stream)})
						return
					}
					var request computerv0.PreparationControlRequest
					if err := frameio.ReadProtoFrame(stream, &request); err != nil {
						t.Error(err)
						return
					}
					if request.Start != nil {
						backend.authored.Add(1)
					}
					response := &computerv0.PreparationControlResponse{State: "absent"}
					if backend.authored.Load() != 0 {
						response.State = "running"
						if request.LogStream == "stdout" {
							if request.AcknowledgedThrough != 0 {
								stdoutACK.Add(1)
							}
							response.Log = &computerv0.PreparationLog{Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: observed, Data: []byte("rejected")}
						} else if request.LogStream == "stderr" {
							if request.AcknowledgedThrough != 0 {
								stderrACK.Add(1)
							} else {
								response.Log = &computerv0.PreparationLog{Kind: "end", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: observed, Complete: true}
							}
						}
						if stderrACK.Load() != 0 && stdoutCalls.Load() != 0 {
							response.State = "succeeded"
						}
					}
					_ = frameio.WriteProtoFrame(stream, response)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				if err := owner.Run(ctx); err != nil {
					t.Fatal(err)
				}
				if stdoutCalls.Load() != 1 || stderrCalls.Load() != 1 || stdoutACK.Load() != 0 || stderrACK.Load() != 1 || client.publications.Load() != 1 || !owner.Finished() {
					t.Fatalf("uploads=(%d,%d) ACK=(%d,%d) publication=%d", stdoutCalls.Load(), stderrCalls.Load(), stdoutACK.Load(), stderrACK.Load(), client.publications.Load())
				}
			})
		})
	}
}

type preparationLogRoundTrip func(*http.Request) (*http.Response, error)

func (f preparationLogRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type preparationLogBrokenBody struct{}

func (preparationLogBrokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestPreparationMalformedDiagnosticsNeverAdvanceGuest(t *testing.T) {
	for _, name := range []string{"sequence", "payload", "kind", "receipt-range", "receipt-expiry", "expired-clock"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				owner, client, backend := preparationOwnerFixture(t)
				owner.machine = backend.machine
				owner.executor = workerapi.PreparationExecutor{Identity: owner.identity, ChannelCredential: bytes.Repeat([]byte{11}, 32)}
				owner.expires.Store(time.Now().Add(time.Minute).UnixNano())
				var polls, uploads atomic.Int64
				backend.machine.handle = func(stream net.Conn) {
					if _, _, err := wire.ReadStreamFrameHeader(stream); err != nil {
						t.Error(err)
						return
					}
					var request computerv0.PreparationControlRequest
					if err := frameio.ReadProtoFrame(stream, &request); err != nil {
						t.Error(err)
						return
					}
					polls.Add(1)
					if request.AcknowledgedThrough != 0 {
						t.Errorf("malformed exchange was acknowledged: %d", request.AcknowledgedThrough)
					}
					log := &computerv0.PreparationLog{Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: time.Now().UnixNano(), Data: []byte("x")}
					switch name {
					case "sequence":
						log.Sequence = 2
						log.ThroughSequence = 2
					case "payload":
						log.Data = bytes.Repeat([]byte("x"), 1025)
					case "kind":
						log.Kind = "unknown"
					}
					_ = frameio.WriteProtoFrame(stream, &computerv0.PreparationControlResponse{State: "running", Log: log})
				}
				client.appendLog = func(_ context.Context, request workerapi.PreparationLogRequest) (workerapi.DiagnosticLogReceipt, error) {
					uploads.Add(1)
					now := time.Now()
					receipt := workerapi.DiagnosticLogReceipt{ThroughSequence: request.ThroughSequence, AcceptedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour)}
					switch name {
					case "receipt-range":
						receipt.ThroughSequence++
					case "receipt-expiry":
						receipt.ExpiresAt = now
					case "expired-clock":
						receipt.Expired = true
					}
					return receipt, nil
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				owner.deliverPreparationLogs(ctx, "stdout")
				expected := int64(1)
				if name == "sequence" || name == "payload" || name == "kind" {
					expected = 0
				}
				if ctx.Err() != nil || polls.Load() != 1 || uploads.Load() != expected {
					t.Fatalf("polls=%d uploads=%d ctx=%v", polls.Load(), uploads.Load(), ctx.Err())
				}
			})
		})
	}
}
