package computerhost

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

type agentControlMachine struct {
	running atomic.Bool
	handle  func(net.Conn)
}

func (m *agentControlMachine) Stream() vm.Stream { return nil }
func (m *agentControlMachine) OpenStream(context.Context) (vm.Stream, error) {
	host, guest := net.Pipe()
	go func() { defer guest.Close(); m.handle(guest) }()
	return host, nil
}
func (*agentControlMachine) Wait(context.Context) error  { return nil }
func (*agentControlMachine) Close(context.Context) error { return errors.New("must not close machine") }
func (m *agentControlMachine) WithRunningGuestControl(ctx context.Context, _ vm.GuestControlStage, run func(context.Context) error) error {
	if !m.running.CompareAndSwap(false, true) {
		return errors.New("overlapping control")
	}
	defer m.running.Store(false)
	return run(ctx)
}

type agentContinuationFixture struct {
	t                                         *testing.T
	mu                                        sync.Mutex
	machine                                   *agentControlMachine
	installation                              *agentv1.ComputerSessionInstallation
	steps                                     []string
	installed, started, activated, committed  bool
	failCommit, loseActivation                bool
	failControls                              bool
	failCurrentControls, wrongControlsReceipt bool
	controlsApplied                           bool
}

func (f *agentContinuationFixture) ReconcileControls(context.Context, *agentv1.ComputerSessionInstallation, *agentv1.ComputerSessionReceipt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, "controls")
	if !f.activated || f.failControls {
		return errors.New("current control reconciliation failed")
	}
	return nil
}

func newAgentContinuationFixture(t *testing.T) *agentContinuationFixture {
	capture := &agentv1.ComputerSessionCapture{Envelope: &computerv0.ComputerOperationEnvelope{ComputerId: "computer"}, CheckpointId: "checkpoint", DesiredVersion: 1}
	f := &agentContinuationFixture{t: t, installation: &agentv1.ComputerSessionInstallation{Capture: capture, Envelope: &computerv0.ComputerOperationEnvelope{ComputerId: "computer", ComputerInstanceId: "instance", WriterGeneration: 2, ChannelCredential: "private", OperationId: "checkpoint"}, DesiredVersion: 2}}
	f.machine = &agentControlMachine{handle: f.handle}
	return f
}
func (f *agentContinuationFixture) AttachSessions(context.Context, *agentv1.ComputerSessionInstallation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, "attach")
	if !f.installed {
		return errors.New("attachments precede installation")
	}
	return nil
}
func (f *agentContinuationFixture) ValidateTarget(_ context.Context, _ *agentv1.ComputerSessionInstallation, observed *agentv1.ComputerSessionReceipt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.committed && !observed.GetInstalled() {
		return errors.New("committed continuation cannot reload old image")
	}
	return nil
}
func (f *agentContinuationFixture) PrepareActivation(_ context.Context, request *agentv1.ComputerSessionInstallation, observed *agentv1.ComputerSessionReceipt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, "commit")
	if f.failCommit {
		return errors.New("commit failed")
	}
	if !observed.GetInstalled() || observed.GetDesiredVersion() != request.GetDesiredVersion() {
		return errors.New("commit requires exact installed receipt")
	}
	f.committed = true
	return nil
}
func (f *agentContinuationFixture) handle(stream net.Conn) {
	f.t.Helper()
	if !f.machine.running.Load() {
		f.t.Error("exchange escaped pause exclusion")
	}
	if _, _, err := wire.ReadStreamFrameHeader(stream); err != nil {
		f.t.Error(err)
		return
	}
	var request agentv1.ComputerSessionControl
	if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &request); err != nil {
		f.t.Error(err)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	response := &agentv1.ComputerSessionReceipt{CheckpointId: "checkpoint", DesiredVersion: 1, Frozen: true, Installed: f.installed, ActivationStarted: f.started, Activated: f.activated}
	if f.installed {
		response.DesiredVersion = 2
	}
	switch request.GetOperation().(type) {
	case *agentv1.ComputerSessionControl_Inspect:
		f.steps = append(f.steps, "inspect")
	case *agentv1.ComputerSessionControl_Install:
		f.steps = append(f.steps, "install")
		nonce := make([]byte, 32)
		nonce[0] = 42
		if err := frameio.WriteProtoFrame(stream, &agentv1.ComputerAuthorityChallenge{Nonce: nonce}); err != nil {
			f.t.Error(err)
			return
		}
		var observed agentv1.ComputerAuthorityObservation
		if err := frameio.ReadProtoFrameBounded(stream, 256, &observed); err != nil {
			f.t.Error(err)
			return
		}
		if !slices.Equal(nonce, observed.Nonce) || time.Since(time.Unix(0, observed.AuthorityTimeUnixNano)) > time.Second {
			f.t.Error("host returned stale or unbound authority time")
		}
		f.installed = true
		response.Installed = true
		response.DesiredVersion = 2
	case *agentv1.ComputerSessionControl_Controls:
		f.steps = append(f.steps, "apply-controls")
		if !f.committed {
			f.t.Error("control application preceded consumption")
		}
		raw, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(request.GetControls())
		digest := sha256.Sum256(raw)
		response.ControlsDigest = digest[:]
		if f.wrongControlsReceipt {
			response.ControlsDigest = []byte("wrong")
		}
		f.controlsApplied = true
	case *agentv1.ComputerSessionControl_Activate:
		if !f.controlsApplied {
			f.t.Error("activation preceded current controls")
		}
		f.steps = append(f.steps, "activate")
		if !f.committed {
			f.t.Error("activation preceded durable exclusion")
		}
		f.started = true
		if f.loseActivation {
			f.loseActivation = false
			return
		}
		f.activated = true
		response.ActivationStarted = true
		response.Activated = true
		response.Frozen = false
	default:
		f.t.Error("unexpected control operation")
	}
	if err := frameio.WriteProtoFrame(stream, response); err != nil {
		f.t.Error(err)
	}
}

func TestAgentComputerContinuationCommitsBeforeActivation(t *testing.T) {
	for _, kind := range []string{"restore", "source-abort", "commit-failure"} {
		t.Run(kind, func(t *testing.T) {
			f := newAgentContinuationFixture(t)
			f.installation.SourceAbort = kind == "source-abort"
			f.failCommit = kind == "commit-failure"
			receipt, err := ContinueAgentComputer(t.Context(), f.machine, f.installation, f)
			failed := f.failCommit
			if (err != nil) != failed {
				t.Fatalf("continuation: %v", err)
			}
			if !failed && !receipt.GetActivated() {
				t.Fatal("continuation did not activate")
			}
			expected := []string{"inspect", "install", "attach", "commit"}
			if !failed {
				expected = append(expected, "read-controls", "apply-controls", "activate", "controls")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if !slices.Equal(f.steps, expected) {
				t.Fatalf("phases %v, expected %v", f.steps, expected)
			}
		})
	}
}

func TestAgentComputerContinuationLostActivationRetriesCurrentProcesses(t *testing.T) {
	f := newAgentContinuationFixture(t)
	f.loseActivation = true
	if _, err := ContinueAgentComputer(t.Context(), f.machine, f.installation, f); !errors.Is(err, io.EOF) {
		t.Fatalf("lost activation: %v", err)
	}
	receipt, err := ContinueAgentComputer(t.Context(), f.machine, f.installation, f)
	if err != nil || !receipt.GetActivated() {
		t.Fatalf("retry: %v %v", receipt, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	expected := []string{"inspect", "install", "attach", "commit", "read-controls", "apply-controls", "activate", "inspect", "install", "attach", "commit", "read-controls", "apply-controls", "activate", "controls"}
	if !slices.Equal(f.steps, expected) {
		t.Fatalf("retry phases %v", f.steps)
	}
}

func TestAgentComputerContinuationWaitsForCurrentControls(t *testing.T) {
	f := newAgentContinuationFixture(t)
	f.failControls = true
	receipt, err := ContinueAgentComputer(t.Context(), f.machine, f.installation, f)
	if err == nil || !receipt.GetActivated() {
		t.Fatalf("failed control reconciliation lost activation state: %v %v", receipt, err)
	}
	f.failControls = false
	if _, err := ContinueAgentComputer(t.Context(), f.machine, f.installation, f); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.steps[len(f.steps)-1] != "controls" {
		t.Fatal("retry omitted current control reconciliation")
	}
}

func TestAgentComputerControlCancellationReleasesPauseExclusion(t *testing.T) {
	for _, stage := range []string{"request", "challenge", "response"} {
		t.Run(stage, func(t *testing.T) {
			reached := make(chan struct{})
			exited := make(chan struct{})
			release := make(chan struct{})
			machine := &agentControlMachine{handle: func(stream net.Conn) {
				defer close(exited)
				if stage != "request" {
					if _, _, err := wire.ReadStreamFrameHeader(stream); err != nil {
						return
					}
					var request agentv1.ComputerSessionControl
					if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &request); err != nil {
						return
					}
					if stage == "response" {
						if err := frameio.WriteProtoFrame(stream, &agentv1.ComputerAuthorityChallenge{Nonce: make([]byte, 32)}); err != nil {
							return
						}
						var observation agentv1.ComputerAuthorityObservation
						if err := frameio.ReadProtoFrameBounded(stream, 256, &observation); err != nil {
							return
						}
					}
				}
				close(reached)
				if stage == "request" {
					<-release
					return
				}
				var b [1]byte
				_, _ = stream.Read(b[:])
			}}
			ctx, cancel := context.WithCancel(t.Context())
			result := make(chan error, 1)
			go func() {
				_, err := controlAgentComputer(ctx, machine, &agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Install{Install: &agentv1.ComputerSessionInstallation{}}})
				result <- err
			}()
			<-reached
			cancel()
			close(release)
			if err := <-result; err == nil {
				t.Fatal("cancelled exchange succeeded")
			}
			<-exited
			if machine.running.Load() {
				t.Fatal("cancellation retained lifecycle exclusion")
			}
		})
	}
}

func TestAgentComputerContinuationRetriesAfterFailedCommit(t *testing.T) {
	f := newAgentContinuationFixture(t)
	f.failCommit = true
	if _, err := ContinueAgentComputer(t.Context(), f.machine, f.installation, f); err == nil {
		t.Fatal("failed commit activated")
	}
	f.failCommit = false
	receipt, err := ContinueAgentComputer(t.Context(), f.machine, f.installation, f)
	if err != nil || !receipt.GetActivated() {
		t.Fatalf("installed retry: %v %v", receipt, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	expected := []string{"inspect", "install", "attach", "commit", "inspect", "install", "attach", "commit", "read-controls", "apply-controls", "activate", "controls"}
	if !slices.Equal(f.steps, expected) {
		t.Fatalf("retry phases %v", f.steps)
	}

}

func TestAgentComputerContinuationRejectsOldImageAfterCommit(t *testing.T) {
	f := newAgentContinuationFixture(t)
	f.committed = true
	for range 2 {
		if _, err := ContinueAgentComputer(t.Context(), f.machine, f.installation, f); err == nil {
			t.Fatal("committed installation activated an old image")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.installed || f.started || slices.Contains(f.steps, "activate") {
		t.Fatal("old image crossed activation boundary")
	}
}

func TestAgentComputerReceiptsCannotGrantMissingAuthority(t *testing.T) {
	installation := newAgentContinuationFixture(t).installation
	request := &agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Activate{Activate: installation}}
	valid := &agentv1.ComputerSessionReceipt{CheckpointId: "checkpoint", DesiredVersion: 2, Installed: true, ActivationStarted: true, Activated: true}
	for _, field := range []string{"checkpoint", "version", "installed", "started", "activated"} {
		t.Run(field, func(t *testing.T) {
			receipt := proto.Clone(valid).(*agentv1.ComputerSessionReceipt)
			switch field {
			case "checkpoint":
				receipt.CheckpointId = "other"
			case "version":
				receipt.DesiredVersion++
			case "installed":
				receipt.Installed = false
			case "started":
				receipt.ActivationStarted = false
			case "activated":
				receipt.Activated = false
			}
			if err := validateAgentComputerReceipt(request, receipt); err == nil {
				t.Fatal("accepted incomplete or mismatched activation")
			}
		})
	}
	failed := proto.Clone(valid).(*agentv1.ComputerSessionReceipt)
	failed.Activated = false
	failed.Error = "refreeze failed"
	if err := validateAgentComputerReceipt(request, failed); err != nil {
		t.Fatal("lost valid uncertain activation receipt", err)
	}
	failed.CheckpointId = ""
	if err := validateAgentComputerReceipt(request, failed); err == nil {
		t.Fatal("accepted unbound error receipt")
	}
}

func TestAgentComputerControlRejectsMalformedChallenge(t *testing.T) {
	machine := &agentControlMachine{handle: func(stream net.Conn) {
		if _, _, err := wire.ReadStreamFrameHeader(stream); err != nil {
			return
		}
		var request agentv1.ComputerSessionControl
		if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &request); err != nil {
			return
		}
		_ = frameio.WriteProtoFrame(stream, &agentv1.ComputerAuthorityChallenge{Nonce: []byte("short")})
	}}
	request := &agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Install{Install: newAgentContinuationFixture(t).installation}}
	if _, err := controlAgentComputer(t.Context(), machine, request); err == nil {
		t.Fatal("accepted malformed authority challenge")
	}
}

type agentPreparationHold struct {
	vm.CheckpointCapture
	entered bool
	machine vm.Machine
}

func (h *agentPreparationHold) PrepareGuest(ctx context.Context, exchange func(context.Context, vm.Stream) error) error {
	h.entered = true
	defer func() { h.entered = false }()
	stream, err := h.machine.OpenStream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	return exchange(ctx, stream)
}

func TestAgentComputerCaptureUsesCheckpointHold(t *testing.T) {
	for _, outcome := range []string{"frozen", "lost-reply", "wrong-checkpoint", "not-frozen", "rejected", "retained-error"} {
		t.Run(outcome, func(t *testing.T) {
			hold := &agentPreparationHold{}
			request := &agentv1.ComputerSessionCapture{CheckpointId: "checkpoint", DesiredVersion: 1}
			machine := &agentControlMachine{handle: func(stream net.Conn) {
				if !hold.entered {
					t.Error("freeze escaped checkpoint hold")
				}
				header, _, err := wire.ReadStreamFrameHeader(stream)
				if err != nil || header.Type != wire.StreamTypeAgentComputer {
					t.Errorf("header: %v %v", header, err)
					return
				}
				var got agentv1.ComputerSessionControl
				if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &got); err != nil {
					t.Error(err)
					return
				}
				if !proto.Equal(got.GetCapture(), request) {
					t.Error("capture request changed")
				}
				if outcome == "lost-reply" {
					return
				}
				receipt := &agentv1.ComputerSessionReceipt{CheckpointId: "checkpoint", DesiredVersion: 1, Frozen: true}
				if outcome == "rejected" {
					receipt = &agentv1.ComputerSessionReceipt{Error: "busy"}
				}
				if outcome == "retained-error" {
					receipt.Error = "freeze incomplete"
				}
				if outcome == "wrong-checkpoint" {
					receipt.CheckpointId = "other"
				}
				if outcome == "not-frozen" {
					receipt.Frozen = false
				}
				if err := frameio.WriteProtoFrame(stream, receipt); err != nil {
					t.Error(err)
				}
			}}
			hold.machine = machine
			receipt, err := CaptureAgentComputer(t.Context(), hold, request)
			if (err == nil) != (outcome == "frozen") {
				t.Fatalf("receipt=%v error=%v", receipt, err)
			}
			var unsealed *agentComputerUnsealedError
			if errors.As(err, &unsealed) != (outcome == "rejected") {
				t.Fatalf("incorrect absence evidence: %v", err)
			}
			if outcome == "retained-error" && receipt == nil {
				t.Fatal("lost retained receipt")
			}
		})
	}
}

func (f *agentContinuationFixture) CurrentControls(_ context.Context, p *agentv1.ComputerSessionInstallation, _ *agentv1.ComputerSessionReceipt) (*agentv1.ComputerSessionControls, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, "read-controls")
	if f.failCurrentControls {
		return nil, errors.New("current controls unavailable")
	}
	return &agentv1.ComputerSessionControls{Envelope: proto.Clone(p.Envelope).(*computerv0.ComputerOperationEnvelope), CheckpointId: p.Capture.CheckpointId, DesiredVersion: p.DesiredVersion}, nil
}
func TestAgentComputerContinuationRequiresPreActivationControlReceipt(t *testing.T) {
	for _, kind := range []string{"read-failure", "wrong-receipt"} {
		t.Run(kind, func(t *testing.T) {
			f := newAgentContinuationFixture(t)
			f.failCurrentControls = kind == "read-failure"
			f.wrongControlsReceipt = kind == "wrong-receipt"
			if _, err := ContinueAgentComputer(t.Context(), f.machine, f.installation, f); err == nil {
				t.Fatal("missing current controls accepted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.started || slices.Contains(f.steps, "activate") {
				t.Fatal("customer activated without current control receipt")
			}
			if !f.committed {
				t.Fatal("test did not reach consumed current installation")
			}
		})
	}
}
