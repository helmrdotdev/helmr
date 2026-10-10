package controlplane

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/computerhost"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"google.golang.org/protobuf/proto"
)

type shortOwnerAttachment struct{ *workerclient.Client }

func (c shortOwnerAttachment) AcquireAgentAttachment(ctx context.Context, s workerapi.RuntimeSession) (workerapi.AgentAttachmentResponse, error) {
	a, err := c.Client.AcquireAgentAttachment(ctx, s)
	if err == nil && !a.Stopped {
		a.ExpiresAt = time.Now().Add(2 * time.Second)
	}
	return a, err
}

func TestWorkerAgentComputerOwnerRetainsAuthorityAndReconcilesLostReplies(t *testing.T) {
	for _, scenario := range []string{"reconnect", "control-failure", "already-stopped", "request-cancel", "close-before-activation", "control-failure-lost-receipt", "control-failure-close", "cancel-waiter", "control-failure-unrecorded", "target-reconnect", "target-stopped", "control-failure-close-owner", "control-failure-concurrent-rejection", "control-failure-report-unavailable", "control-failure-report-lost"} {
		t.Run(scenario, func(t *testing.T) {
			controlFailure := strings.HasPrefix(scenario, "control-failure")
			stopped := strings.HasSuffix(scenario, "stopped")
			target := strings.HasPrefix(scenario, "target-")
			f := agenttest.New(t)
			handler := newPostgresServer(t, f.Pool)
			var p *agentv1.ComputerSessionInstallation
			var activated, installed atomic.Bool
			var attachments, controls atomic.Int32
			renewed := make(chan struct{})
			commitEntered := make(chan struct{})
			receiptEntered := make(chan struct{})
			receiptRelease := make(chan struct{})
			var receiptOnce, releaseReceiptOnce sync.Once
			unblockReceipt := func() { releaseReceiptOnce.Do(func() { close(receiptRelease) }) }
			commitRelease := make(chan struct{})
			var releaseCommit sync.Once
			unblockCommit := func() { releaseCommit.Do(func() { close(commitRelease) }) }
			var renewalOnce sync.Once
			var firstCommit sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if (r.URL.Path == "/worker/v1/agent-computers/source-abort/commit" || r.URL.Path == "/worker/v1/agent-computers/restore/commit") && !stopped {
					firstCommit.Do(func() {
						close(commitEntered)
						if scenario == "request-cancel" || scenario == "close-before-activation" || scenario == "cancel-waiter" {
							<-commitRelease
							return
						}
						select {
						case <-renewed:
						case <-r.Context().Done():
						}
					})
				}
				if r.URL.Path == "/worker/v1/sessions/control" && !activated.Load() {
					t.Error("ordinary controls preceded activation")
				}

				if r.URL.Path == "/worker/v1/sessions/failed" && (scenario == "control-failure-report-unavailable" || scenario == "control-failure-report-lost") {
					if scenario == "control-failure-report-lost" {
						recorded := httptest.NewRecorder()
						handler.ServeHTTP(recorded, r)
						if recorded.Code != http.StatusOK {
							t.Errorf("failure report commit: %d", recorded.Code)
						}
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if r.URL.Path == "/worker/v1/sessions/control/receipt" && scenario == "control-failure-unrecorded" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if r.URL.Path == "/worker/v1/sessions/control/receipt" && scenario == "control-failure-lost-receipt" {
					recorded := httptest.NewRecorder()
					handler.ServeHTTP(recorded, r)
					if recorded.Code != http.StatusOK {
						t.Errorf("recording failed receipt: %d", recorded.Code)
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}

				if scenario == "control-failure-close-owner" || scenario == "control-failure-concurrent-rejection" {
					if r.URL.Path == "/worker/v1/sessions/control/receipt" {
						receiptOnce.Do(func() { close(receiptEntered) })
						<-receiptRelease
					}
					if r.URL.Path == "/worker/v1/sessions/control" && scenario == "control-failure-concurrent-rejection" {
						select {
						case <-receiptEntered:
							w.WriteHeader(http.StatusConflict)
							return
						default:
						}
					}
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			defer unblockReceipt()
			defer unblockCommit()
			client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
			request, captureResult, capture := beginWorkerAgentCapture(t, f, client)
			observed := &agentv1.ComputerSessionReceipt{CheckpointId: capture.CheckpointId, DesiredVersion: capture.DesiredVersion, Frozen: true}
			if err := client.SealAgentComputerCapture(t.Context(), workerapi.AgentComputerReceiptRequest{EnvironmentID: request.EnvironmentID, CheckpointID: request.CheckpointID, Receipt: agentComputerBytes(t, observed)}); err != nil {
				t.Fatal(err)
			}
			if stopped {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET status='cancelled',authority_generation=2 WHERE id=$1`, f.Session)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1`, f.Session)
			}
			var prepared workerapi.AgentComputerInstallationResponse
			var err error
			if target {
				restore, _ := seedWorkerAgentRestoreTarget(t, f, request, captureResult)
				prepared, err = client.PrepareAgentComputerRestore(t.Context(), restore)
			} else {
				prepared, err = client.PrepareAgentComputerSourceAbort(t.Context(), workerapi.AgentComputerSourceAbortRequest{EnvironmentID: request.EnvironmentID, CheckpointID: request.CheckpointID, LeaseEpoch: 1, ChannelCredential: agenttest.ChannelCredential, Receipt: agentComputerBytes(t, observed)})
			}
			if err != nil {
				t.Fatal(err)
			}
			p = new(agentv1.ComputerSessionInstallation)
			if err := proto.Unmarshal(prepared.Installation, p); err != nil {
				t.Fatal(err)
			}
			var loseActivation atomic.Bool
			loseActivation.Store(true)
			machine := &workerAgentTestMachine{handle: func(stream net.Conn) {
				header, _, err := wire.ReadStreamFrameHeader(stream)
				if err != nil {
					return
				}
				switch header.Type {
				case wire.StreamTypeAgentComputer:
					var command agentv1.ComputerSessionControl
					if err := frameio.ReadProtoFrameBounded(stream, 1<<20, &command); err != nil {
						t.Error(err)
						return
					}
					receipt := &agentv1.ComputerSessionReceipt{CheckpointId: capture.CheckpointId, DesiredVersion: capture.DesiredVersion, Frozen: !activated.Load(), Installed: installed.Load(), ActivationStarted: activated.Load(), Activated: activated.Load()}
					if installed.Load() {
						receipt.DesiredVersion = p.DesiredVersion
					}
					switch {
					case command.GetInstall() != nil:
						nonce := make([]byte, 32)
						nonce[0] = 1
						if err := frameio.WriteProtoFrame(stream, &agentv1.ComputerAuthorityChallenge{Nonce: nonce}); err != nil {
							return
						}
						var reply agentv1.ComputerAuthorityObservation
						if err := frameio.ReadProtoFrameBounded(stream, 256, &reply); err != nil {
							return
						}
						installed.Store(true)
						receipt.Installed = true
						receipt.DesiredVersion = p.DesiredVersion
					case command.GetControls() != nil:
						raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(command.GetControls())
						digest := sha256.Sum256(raw)
						receipt.ControlsDigest = digest[:]
					case command.GetActivate() != nil:
						activated.Store(true)
						if loseActivation.CompareAndSwap(true, false) {
							return
						}
						receipt.Installed = true
						receipt.ActivationStarted = true
						receipt.Activated = true
						receipt.Frozen = false
						receipt.DesiredVersion = p.DesiredVersion
					}
					_ = frameio.WriteProtoFrame(stream, receipt)
				case wire.StreamTypeAgentSession:
					var attach agentv1.SessionAttach
					if err := frameio.ReadProtoFrameBounded(stream, 1<<20, &attach); err != nil {
						return
					}
					if attach.Start != nil {
						t.Error("retained owner restarted setup")
						return
					}
					attempt := attachments.Add(1)
					identity := attach.Grant.Identity
					if err := frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: identity, AttachmentSequence: attach.AttachmentSequence, Message: &agentv1.GuestSessionMessage_Attached{Attached: &agentv1.SessionAttached{Ready: true}}}); err != nil {
						return
					}
					for {
						var message agentv1.HostSessionMessage
						if err := frameio.ReadProtoFrameBounded(stream, 1<<20, &message); err != nil {
							return
						}
						if message.GetRenew() != nil {
							renewalOnce.Do(func() { close(renewed) })
							continue
						}
						if command := message.GetCommand(); command != nil {
							if !activated.Load() {
								t.Error("command preceded activation")
								return
							}
							if command.GetResume() == nil {
								t.Error("unexpected owner control")
								return
							}
							controls.Add(1)
							if attempt == 1 && strings.HasSuffix(scenario, "reconnect") {
								return
							} // An uncertain delivery closes only the attachment.
							result := &agentv1.DeliveryResult{DeliveryId: command.DeliveryId, Outcome: &agentv1.DeliveryResult_ValueJson{ValueJson: []byte(`null`)}}
							if controlFailure {
								result.Outcome = &agentv1.DeliveryResult_Error{Error: &agentv1.OperationError{Code: "reconstruction_required", Message: "interrupted turn requires reconstruction"}}
							}
							if err := frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: identity, AttachmentSequence: attach.AttachmentSequence, EventSequence: 1, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: identity, Event: &agentv1.ProgramEvent_DeliveryResult{DeliveryResult: result}}}}); err != nil {
								return
							}
							if scenario == "control-failure-close" {
								return
							}
						}
					}
				default:
					t.Error("unexpected guest stream")
				}
			}}
			owner, err := computerhost.NewAgentComputerOwner(t.Context(), machine, shortOwnerAttachment{client}, request.EnvironmentID, p, func(context.Context, *agentv1.GuestSessionMessage) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()

			if scenario == "request-cancel" || scenario == "close-before-activation" {
				requestCtx, cancelRequest := context.WithCancel(ctx)
				done := make(chan error, 1)
				go func() { _, err := owner.Continue(requestCtx); done <- err }()
				select {
				case <-commitEntered:
				case <-ctx.Done():
					t.Fatal("commit did not start")
				}
				if scenario == "close-before-activation" {
					if err := owner.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					cancelRequest()
				}
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancelled request: %v", err)
					}
				case <-ctx.Done():
					t.Fatal("continuation did not cancel")
				}
				cancelRequest()
				unblockCommit()
				if controls.Load() != 0 {
					t.Fatal("cancellation released controls")
				}
				if scenario == "close-before-activation" {
					return
				}
				if owner.Err() != nil {
					t.Fatalf("request cancelled retained owner: %v", owner.Err())
				}
			}

			if scenario == "cancel-waiter" {
				first := make(chan error, 1)
				go func() { _, err := owner.Continue(ctx); first <- err }()
				select {
				case <-commitEntered:
				case <-ctx.Done():
					t.Fatal("first continuation did not enter commit")
				}
				waiterCtx, cancelWaiter := context.WithCancel(ctx)
				waiter := make(chan error, 1)
				go func() { _, err := owner.Continue(waiterCtx); waiter <- err }()
				cancelWaiter()
				select {
				case err := <-waiter:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("waiting continuation: %v", err)
					}
				case <-ctx.Done():
					t.Fatal("cancelled waiter blocked behind active continuation")
				}
				unblockCommit()
				select {
				case err := <-first:
					if !errors.Is(err, io.EOF) {
						t.Fatalf("first continuation: %v", err)
					}
				case <-ctx.Done():
					t.Fatal("first continuation did not finish")
				}
			} else {
				if _, err := owner.Continue(ctx); !errors.Is(err, io.EOF) {
					t.Fatalf("uncertain activation: %v", err)
				}
			}
			if controls.Load() != 0 {
				t.Fatal("lost activation receipt released control gate")
			}

			if scenario == "control-failure-close-owner" {
				done := make(chan error, 1)
				go func() { _, err := owner.Continue(ctx); done <- err }()
				select {
				case <-receiptEntered:
				case <-ctx.Done():
					t.Fatal("failed control receipt did not begin")
				}
				var failed computerhost.SessionControlFailedError
				if err := owner.Close(); !errors.As(err, &failed) || failed.Session.SessionID != f.Session.String() {
					t.Fatalf("Close erased known failed control: %+v %v", failed, err)
				}
				if errors.Is(failed, context.Canceled) || errors.Is(failed, context.DeadlineExceeded) {
					t.Fatal("receipt cancellation masqueraded as runtime cancellation")
				}
				unblockReceipt()
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("closed owner did not join request")
				}
				if attachments.Load() != 1 {
					t.Fatal("Close retried failed process")
				}
				return
			}
			receipt, err := owner.Continue(ctx)
			if controlFailure {
				var failed computerhost.SessionControlFailedError
				if !errors.As(err, &failed) || attachments.Load() != 1 || controls.Load() != 1 {
					t.Fatalf("failed control retried: attachments=%d controls=%d error=%v", attachments.Load(), controls.Load(), err)
				}

				if failed.Session.SessionID != f.Session.String() || failed.Session.ProcessEpoch != 1 {
					t.Fatalf("failure lost process identity: %+v", failed)
				}
				if failed.AttachmentSequence <= 0 {
					t.Fatal("failure lost attachment identity")
				}
				reportUnknown := scenario == "control-failure-report-unavailable" || scenario == "control-failure-report-lost"
				if (failed.FailureError != nil) != reportUnknown {
					t.Fatalf("failure report outcome: %v", failed.FailureError)
				}
				var recorded, stopping bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT failure_recorded_at IS NOT NULL,status='stopping' AND fenced_at IS NULL FROM session_processes WHERE session_id=$1`, f.Session).Scan(&recorded, &stopping); err != nil {
					t.Fatal(err)
				}
				if scenario != "control-failure-report-unavailable" && (!recorded || !stopping) {
					t.Fatalf("failure did not persist unfenced stop: %v %v", recorded, stopping)
				}
				if scenario == "control-failure-report-unavailable" && recorded {
					t.Fatal("failed report claimed durable failure")
				}
				if scenario == "control-failure-unrecorded" && failed.ReceiptError == nil {
					t.Fatal("lost failure receipt outcome was hidden")
				}
				var rejected bool
				if scenario == "control-failure-unrecorded" || scenario == "control-failure-concurrent-rejection" {
					if err := f.Pool.QueryRow(t.Context(), `SELECT controls_reconciled_at IS NULL FROM computer_checkpoints WHERE id=$1`, capture.CheckpointId).Scan(&rejected); err != nil || !rejected {
						t.Fatalf("uncertain failure released continuation: %v %v", rejected, err)
					}
					return
				}
				if err := f.Pool.QueryRow(t.Context(), `SELECT cp.controls_reconciled_at IS NULL AND p.control_error IS NOT NULL FROM computer_checkpoints cp JOIN computer_checkpoint_members m ON m.checkpoint_id=cp.id JOIN session_processes p ON p.session_id=m.session_id AND p.epoch=m.process_epoch WHERE cp.id=$1`, capture.CheckpointId).Scan(&rejected); err != nil || !rejected {
					t.Fatalf("failure was not durable: %v %v", rejected, err)
				}
				return
			}
			if err != nil || !receipt.GetActivated() {
				t.Fatalf("reconciled owner: %+v %v", receipt, err)
			}
			expected := int32(2)
			if scenario == "request-cancel" || scenario == "cancel-waiter" {
				expected = 1
			}
			if stopped {
				expected = 0
			}
			if attachments.Load() != expected || controls.Load() != expected {
				t.Fatalf("attachment retry: %d attachments, %d controls", attachments.Load(), controls.Load())
			}
			var reconciled bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT controls_reconciled_at IS NOT NULL FROM computer_checkpoints WHERE id=$1`, capture.CheckpointId).Scan(&reconciled); err != nil || !reconciled {
				t.Fatalf("completion: %v %v", reconciled, err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-owner.Done():
			default:
				t.Fatal("owner did not join")
			}

		})
	}
}
