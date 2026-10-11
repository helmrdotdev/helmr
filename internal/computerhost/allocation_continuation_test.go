package computerhost

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

type allocationContinuationFixture struct {
	AllocatedComputerClient
	mu                                              sync.Mutex
	t                                               *testing.T
	capture                                         *agentv1.ComputerSessionCapture
	target                                          workerapi.AllocationIdentity
	source                                          bool
	scenario                                        string
	prepare, validate, absence, commit, inspections int
	delayed                                         bool
	installed, activated                            bool
	p                                               *agentv1.ComputerSessionInstallation
}

func (f *allocationContinuationFixture) prepareInstallation() (workerapi.AgentComputerInstallationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepare++
	if f.installed {
		f.t.Fatal("installed request was regenerated")
	}
	p := &agentv1.ComputerSessionInstallation{Capture: f.capture, DesiredVersion: 2, SourceAbort: f.source, BaseComputerDiskVersionId: "base", Envelope: &computerv0.ComputerOperationEnvelope{ComputerId: f.target.OwnerID, ComputerInstanceId: f.target.InstanceID, WriterGeneration: uint64(f.target.Epoch), ChannelCredential: "current", OperationId: f.capture.CheckpointId, OperationExpiresAtUnixNano: time.Now().Add(10 * time.Millisecond).UnixNano()}, Grants: []*agentv1.SessionGrant{{Identity: f.capture.Sessions[0], AuthorityGeneration: int64(f.prepare)}}}
	p.StoppedSessions = []*agentv1.SessionIdentity{p.Grants[0].Identity}
	f.p = p
	raw, err := proto.Marshal(p)
	return workerapi.AgentComputerInstallationResponse{Installation: raw}, err
}
func (f *allocationContinuationFixture) PrepareAgentComputerSourceAbort(context.Context, workerapi.AgentComputerSourceAbortRequest) (workerapi.AgentComputerInstallationResponse, error) {
	return f.prepareInstallation()
}
func (f *allocationContinuationFixture) PrepareAgentComputerRestore(context.Context, workerapi.AgentComputerRestoreRequest) (workerapi.AgentComputerInstallationResponse, error) {
	return f.prepareInstallation()
}
func (f *allocationContinuationFixture) ValidateAgentComputerSourceAbort(context.Context, workerapi.AgentComputerInstallationRequest) error {
	return f.validation()
}
func (f *allocationContinuationFixture) ValidateAgentComputerRestore(context.Context, workerapi.AgentComputerInstallationRequest) error {
	return f.validation()
}
func (f *allocationContinuationFixture) validation() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validate++
	if f.scenario == "stale-before-install" && f.prepare == 1 {
		return &httpclient.Error{StatusCode: http.StatusConflict, Code: workerapi.AgentComputerNotReady}
	}
	return nil
}
func (f *allocationContinuationFixture) AgentComputerSaveAbsence(context.Context, workerapi.AgentComputerSaveAbsenceRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.absence++
	if f.scenario == "absence-lost" && f.absence == 1 {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func (f *allocationContinuationFixture) AcquireAgentAttachment(context.Context, workerapi.RuntimeSession) (workerapi.AgentAttachmentResponse, error) {
	return workerapi.AgentAttachmentResponse{Stopped: true}, nil
}
func (f *allocationContinuationFixture) CommitAgentComputerSourceAbort(context.Context, workerapi.AgentComputerInstallationRequest) error {
	return f.commitInstallation()
}
func (f *allocationContinuationFixture) CommitAgentComputerRestore(context.Context, workerapi.AgentComputerInstallationRequest) error {
	return f.commitInstallation()
}
func (f *allocationContinuationFixture) commitInstallation() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commit++
	if f.scenario == "commit-lost" && f.commit == 1 {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func (f *allocationContinuationFixture) ReadAgentComputerControls(context.Context, workerapi.AgentComputerInstallationRequest) (workerapi.AgentComputerControlsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.p
	raw, err := proto.Marshal(&agentv1.ComputerSessionControls{CheckpointId: p.Capture.CheckpointId, DesiredVersion: p.DesiredVersion, Envelope: p.Envelope})
	return workerapi.AgentComputerControlsResponse{Controls: raw}, err
}
func (f *allocationContinuationFixture) CompleteAgentComputerSourceAbort(context.Context, workerapi.AgentComputerInstallationRequest) error {
	return nil
}
func (f *allocationContinuationFixture) CompleteAgentComputerRestore(context.Context, workerapi.AgentComputerInstallationRequest) error {
	return nil
}
func (f *allocationContinuationFixture) handle(stream net.Conn) {
	if _, _, err := wire.ReadStreamFrameHeader(stream); err != nil {
		return
	}
	var request agentv1.ComputerSessionControl
	if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &request); err != nil {
		return
	}
	if request.GetInstall() != nil {
		if err := frameio.WriteProtoFrame(stream, &agentv1.ComputerAuthorityChallenge{Nonce: make([]byte, 32)}); err != nil {
			return
		}
		var observation agentv1.ComputerAuthorityObservation
		if err := frameio.ReadProtoFrameBounded(stream, 256, &observation); err != nil {
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	receipt := &agentv1.ComputerSessionReceipt{CheckpointId: f.capture.CheckpointId, DesiredVersion: 1, Frozen: !f.activated, Installed: f.installed, ActivationStarted: f.activated, Activated: f.activated}
	switch {
	case request.GetCapture() != nil:
		return // capture reply deliberately lost
	case request.GetInspect() != nil:
		f.inspections++
		switch f.scenario {
		case "absent-capture-pending":
			receipt = &agentv1.ComputerSessionReceipt{Error: "capture absent", ErrorCode: agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_CAPTURE_ADMISSION_PENDING}
		case "absent-capture-expired":
			receipt = &agentv1.ComputerSessionReceipt{Error: "capture absent", ErrorCode: agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_CAPTURE_ABSENT_AFTER_EXPIRY}
		case "unknown-inspection":
			receipt = &agentv1.ComputerSessionReceipt{Error: "inspection rejected"}
		}
		if f.scenario == "lost-install" && f.delayed {
			f.delayed = false
			// The overtaken Install enters after this uninstalled observation.
			defer func() { f.installed = true }()
		}
		if f.scenario == "late-seal" && f.inspections == 1 {
			receipt = &agentv1.ComputerSessionReceipt{Error: "not sealed", ErrorCode: agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_CAPTURE_ADMISSION_PENDING}
		}
	case request.GetInstall() != nil:
		if f.scenario == "lost-install" && !f.installed {
			f.delayed = true
			return
		}
		if f.scenario == "permanent-rejection" || f.scenario == "unusable-capture" {
			receipt.Error = "capture membership changed"
			if f.scenario == "unusable-capture" {
				receipt.ErrorCode = agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_CAPTURE_UNUSABLE
			}
		} else if f.scenario == "expired-at-guest" && f.prepare == 1 {
			receipt.Error = "grants expired"
			receipt.ErrorCode = agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_AUTHORITY_EXPIRED
		} else {
			f.installed = true
			receipt.Installed = true
		}
	case request.GetControls() != nil:
		raw, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(request.GetControls())
		sum := sha256.Sum256(raw)
		receipt.ControlsDigest = sum[:]
	case request.GetActivate() != nil:
		f.activated = true
		receipt.ActivationStarted = true
		receipt.Activated = true
		receipt.Frozen = false
	}
	if f.installed {
		receipt.DesiredVersion = 2
	}
	_ = frameio.WriteProtoFrame(stream, receipt)
}
func TestComputerContinuationRefreshesOnlyUninstalledAuthority(t *testing.T) {
	for _, source := range []bool{true, false} {
		for _, scenario := range []string{"stale-before-install", "expired-at-guest", "commit-lost", "absence-lost", "late-seal", "lost-install", "permanent-rejection", "unusable-capture", "absent-capture-pending", "absent-capture-expired", "unknown-inspection"} {
			if !source && (scenario == "absence-lost" || scenario == "late-seal") {
				continue
			}
			name := "restore/" + scenario
			if source {
				name = "source/" + scenario
			}
			t.Run(name, func(t *testing.T) {
				id := workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: uuid.NewV7().String(), OwnerID: uuid.NewV7().String(), InstanceID: uuid.NewV7().String(), Epoch: 2}
				checkpoint := uuid.NewV7()
				capture := &agentv1.ComputerSessionCapture{CheckpointId: checkpoint.String(), DesiredVersion: 1, Envelope: &computerv0.ComputerOperationEnvelope{OperationExpiresAtUnixNano: time.Now().Add(-time.Second).UnixNano()}, Sessions: []*agentv1.SessionIdentity{{SessionId: uuid.NewV7().String(), ProcessEpoch: 1}}}
				f := &allocationContinuationFixture{t: t, target: id, capture: capture, source: source, scenario: scenario}
				machine := &checkpointPipelineMachine{control: &agentControlMachine{handle: f.handle}}
				o := &ComputerAllocationOwner{identity: id, client: f, machine: machine, checkpoint: &computercheckpoint.Manifest{CheckpointID: checkpoint}, observe: func(context.Context, *agentv1.GuestSessionMessage) error { return nil }}
				delivery := workerapi.ComputerAllocationDelivery{BaseVersion: "base", ChannelCredential: "current"}
				var err error
				if scenario == "late-seal" {
					err = o.abortUncutCheckpoint(t.Context(), delivery, capture, machine)
				} else {
					initial := capture
					if !source {
						initial = nil
					}
					if source {
						err = o.continueCheckpoint(t.Context(), delivery, initial, true)
					} else {
						err = o.activateRestore(t.Context(), delivery)
					}
				}
				if scenario == "permanent-rejection" || scenario == "unusable-capture" || scenario == "absent-capture-pending" || scenario == "absent-capture-expired" || scenario == "unknown-inspection" {
					invalid := (scenario == "unusable-capture" || scenario == "absent-capture-pending" || scenario == "absent-capture-expired") && !source
					if errors.Is(err, errInvalidCheckpoint) != invalid || (o.invalidCheckpointID != "") != invalid {
						t.Fatalf("wrong restore failure classification: %v cp=%q", err, o.invalidCheckpointID)
					}
					expectedPreparations := 1
					if source && (scenario == "absent-capture-pending" || scenario == "absent-capture-expired" || scenario == "unknown-inspection") {
						expectedPreparations = 0
					}
					if err == nil || f.prepare != expectedPreparations || f.activated {
						t.Fatalf("permanent rejection retried or activated: %v prepares=%d", err, f.prepare)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				expected := 2
				if scenario == "commit-lost" || scenario == "late-seal" || scenario == "lost-install" {
					expected = 1
				}
				if f.prepare != expected || !f.activated || machine.snapshots != 0 {
					t.Fatalf("preparations=%d activated=%v snapshots=%d", f.prepare, f.activated, machine.snapshots)
				}
				if scenario == "late-seal" && machine.aborts != 1 {
					t.Fatal("late sealed source was not resumed")
				}
			})
		}
	}
}
