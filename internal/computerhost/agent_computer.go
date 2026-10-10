package computerhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

const agentTransportFrameLimit = 16*1024*1024 + 64*1024

// CaptureAgentComputer seals the complete resident Session set under the VM's
// checkpoint hold. An uncertain response requires retrying the identical capture
// or explicit source abort; it never authorizes snapshot serialization.
func CaptureAgentComputer(ctx context.Context, capture vm.CheckpointCapture, request *agentv1.ComputerSessionCapture) (*agentv1.ComputerSessionReceipt, error) {
	if capture == nil || request == nil {
		return nil, errors.New("computer capture requires a hold and request")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	control := &agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Capture{Capture: request}}
	if proto.Size(control) > agentTransportFrameLimit {
		return nil, errors.New("computer capture request is too large")
	}
	var receipt *agentv1.ComputerSessionReceipt
	err := capture.PrepareGuest(ctx, func(ctx context.Context, stream vm.Stream) error {
		var err error
		receipt, err = exchangeAgentComputer(ctx, stream, control)
		return err
	})
	return receipt, err
}

// controlAgentComputer owns a bounded exchange, including cancellation of every
// read/write and the installation clock challenge. The machine excludes pauses
// until the response has been consumed; a paused guest cannot measure host delay.
func controlAgentComputer(ctx context.Context, machine vm.GuestControlMachine, request *agentv1.ComputerSessionControl) (*agentv1.ComputerSessionReceipt, error) {
	if machine == nil || request == nil || request.GetOperation() == nil || proto.Size(request) > agentTransportFrameLimit {
		return nil, errors.New("computer control request is incomplete or too large")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var receipt *agentv1.ComputerSessionReceipt
	stage := vm.GuestControlOrdinary
	if request.GetInstall() != nil {
		stage = vm.GuestControlInstallation
	}
	if request.GetActivate() != nil {
		stage = vm.GuestControlActivation
	}
	err := machine.WithRunningGuestControl(ctx, stage, func(ctx context.Context) error {
		stream, err := machine.OpenStream(ctx)
		if err != nil {
			return err
		}
		defer stream.Close()
		closed := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { defer close(closed); _ = stream.Close() })
		defer func() {
			if !stop() {
				<-closed
			}
		}()
		receipt, err = exchangeAgentComputer(ctx, stream, request)
		return err
	})
	if err != nil {
		return receipt, errors.Join(err, ctx.Err())
	}
	return receipt, nil
}

// agentComputerUnsealedError is a complete negative receipt from the owned
// guest. Transport, malformed, and retained-record errors never imply absence.
type agentComputerUnsealedError struct {
	reason string
	code   agentv1.ComputerSessionErrorCode
}

func (e *agentComputerUnsealedError) Error() string { return e.reason }

func exchangeAgentComputer(ctx context.Context, stream vm.Stream, request *agentv1.ComputerSessionControl) (*agentv1.ComputerSessionReceipt, error) {
	if _, err := writeGuestControlRequest(stream, wire.StreamHeader{Type: wire.StreamTypeAgentComputer}, request); err != nil {
		return nil, err
	}
	if request.GetInstall() != nil {
		var challenge agentv1.ComputerAuthorityChallenge
		if err := frameio.ReadProtoFrameBounded(stream, 256, &challenge); err != nil {
			return nil, err
		}
		if len(challenge.GetNonce()) != 32 {
			return nil, errors.New("computer authority challenge is invalid")
		}
		observation := &agentv1.ComputerAuthorityObservation{Nonce: challenge.GetNonce(), AuthorityTimeUnixNano: time.Now().UnixNano()}
		if err := frameio.WriteProtoFrame(stream, observation); err != nil {
			return nil, err
		}
	}
	response := new(agentv1.ComputerSessionReceipt)
	if err := frameio.ReadProtoFrameBounded(stream, 64*1024, response); err != nil {
		return nil, err
	}
	invalidRestore := request.GetInstall() != nil && !request.GetInstall().GetSourceAbort() && response.GetErrorCode() == agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_CAPTURE_UNUSABLE
	if err := validateAgentComputerReceipt(request, response); err != nil {
		var unsealed *agentComputerUnsealedError
		if invalidRestore && errors.As(err, &unsealed) {
			return nil, invalidCheckpoint(err)
		}
		return nil, err
	}
	if response.GetError() != "" {
		err := fmt.Errorf("guest Computer control: %s", response.GetError())
		if invalidRestore {
			err = invalidCheckpoint(err)
		}
		return response, err
	}
	return response, nil
}

func validateAgentComputerReceipt(request *agentv1.ComputerSessionControl, receipt *agentv1.ComputerSessionReceipt) error {
	var capture *agentv1.ComputerSessionCapture
	var installation *agentv1.ComputerSessionInstallation
	switch operation := request.GetOperation().(type) {
	case *agentv1.ComputerSessionControl_Capture:
		capture = operation.Capture
	case *agentv1.ComputerSessionControl_Inspect:
		capture = operation.Inspect
	case *agentv1.ComputerSessionControl_Controls:
		capture = &agentv1.ComputerSessionCapture{CheckpointId: operation.Controls.GetCheckpointId(), DesiredVersion: operation.Controls.GetDesiredVersion()}
	case *agentv1.ComputerSessionControl_Install:
		installation = operation.Install
		capture = installation.GetCapture()
	case *agentv1.ComputerSessionControl_Activate:
		installation = operation.Activate
		capture = installation.GetCapture()
	}
	version := capture.GetDesiredVersion()
	if installation != nil {
		version = installation.GetDesiredVersion()
	}
	// A rejected operation may precede any retained record. Never turn its flags
	// into authority; the caller receives only an error in this case.
	if receipt.GetError() != "" && receipt.GetCheckpointId() == "" {
		return &agentComputerUnsealedError{reason: receipt.GetError(), code: receipt.GetErrorCode()}
	}
	versionMatches := receipt.GetDesiredVersion() == version
	if request.GetInstall() != nil && receipt.GetError() != "" && !receipt.GetInstalled() {
		// Rejected fresh grants leave the retained capture version unchanged.
		// Preserve this negative receipt for inspection-driven grant refresh.
		versionMatches = receipt.GetDesiredVersion() == capture.GetDesiredVersion()
	}
	if request.GetInspect() != nil || (request.GetCapture() != nil && receipt.GetError() != "") {
		versionMatches = receipt.GetDesiredVersion() >= version
	}
	if capture == nil || receipt.GetCheckpointId() != capture.GetCheckpointId() || !versionMatches {
		return errors.New("computer receipt differs from its request")
	}
	if receipt.GetError() != "" {
		return nil
	}
	switch request.GetOperation().(type) {
	case *agentv1.ComputerSessionControl_Capture:
		if !receipt.GetFrozen() || receipt.GetActivated() {
			return errors.New("computer capture is not frozen")
		}
	case *agentv1.ComputerSessionControl_Install:
		if !receipt.GetInstalled() {
			return errors.New("computer authority was not installed")
		}
	case *agentv1.ComputerSessionControl_Controls:
		raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(request.GetControls())
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		if !receipt.GetInstalled() || receipt.GetActivated() || !bytes.Equal(receipt.GetControlsDigest(), digest[:]) {
			return errors.New("computer current controls were not acknowledged")
		}
	case *agentv1.ComputerSessionControl_Activate:
		if !receipt.GetInstalled() || !receipt.GetActivated() || !receipt.GetActivationStarted() {
			return errors.New("computer continuation was not activated")
		}
	}
	return nil
}

// AgentComputerContinuation belongs to the durable control-plane owner. Prepare
// must commit the exact installation's irreversible activation boundary before
// replying: neither an uncertain guest reply nor a host crash may make the old
// checkpoint eligible again. Installation identity is unique to one physical
// restore. A prior commit with an initially uninstalled guest is an old image,
// not a retry: ValidateTarget must reject it before any guest mutation. Prepare
// validates the supplied guest installation receipt and idempotently commits the
// same current physical installation. AttachSessions preserves valid connections, otherwise
// uses increasing attachment sequences and renews expired grants from current
// authority; it must never run setup or release a hold.
// CurrentControls reads the current complete member state after the activation
// boundary is committed. The host requires its guest acknowledgement before thaw.
// The owner keeps Turn/message dispatch gated through ReconcileControls, which
// reads current durable controls, delivers ordinary sequenced control commands
// and waits for their receipts. A restore-time snapshot cannot stand in for
// controls changed while activation was in flight. Failed reconciliation retries
// on the current live installation; it never authorizes checkpoint replay.
type AgentComputerContinuation interface {
	ValidateTarget(context.Context, *agentv1.ComputerSessionInstallation, *agentv1.ComputerSessionReceipt) error
	AttachSessions(context.Context, *agentv1.ComputerSessionInstallation) error
	PrepareActivation(context.Context, *agentv1.ComputerSessionInstallation, *agentv1.ComputerSessionReceipt) error
	CurrentControls(context.Context, *agentv1.ComputerSessionInstallation, *agentv1.ComputerSessionReceipt) (*agentv1.ComputerSessionControls, error)
	ReconcileControls(context.Context, *agentv1.ComputerSessionInstallation, *agentv1.ComputerSessionReceipt) error
}

// ContinueAgentComputer installs all authority, connects all resident Sessions,
// commits the activation boundary, applies current controls, then permits execution.
// It never runs setup.
func ContinueAgentComputer(ctx context.Context, machine vm.GuestControlMachine, installation *agentv1.ComputerSessionInstallation, owner AgentComputerContinuation) (*agentv1.ComputerSessionReceipt, error) {
	if machine == nil || owner == nil || installation == nil || installation.GetCapture() == nil {
		return nil, errors.New("computer continuation requires an installation and owner")
	}
	// The owner must serialize this continuation with other controls on the same
	// Computer. The guest verifies physical freeze inside installation.
	retained, err := controlAgentComputer(ctx, machine, &agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Inspect{Inspect: installation.GetCapture()}})
	if err != nil {
		var absent *agentComputerUnsealedError
		if !installation.GetSourceAbort() && errors.As(err, &absent) &&
			(absent.code == agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_CAPTURE_ADMISSION_PENDING || absent.code == agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_CAPTURE_ABSENT_AFTER_EXPIRY) {
			// This image was captured only after sealing. On a restore target,
			// positively observed absence cannot be a delayed source capture.
			return retained, invalidCheckpoint(err)
		}
		return retained, err
	}
	if retained.GetInstalled() && retained.GetDesiredVersion() != installation.GetDesiredVersion() {
		return nil, errors.New("computer continuation version changed")
	}
	if err := owner.ValidateTarget(ctx, proto.Clone(installation).(*agentv1.ComputerSessionInstallation), proto.Clone(retained).(*agentv1.ComputerSessionReceipt)); err != nil {
		return retained, err
	}
	receipt, err := controlAgentComputer(ctx, machine, &agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Install{Install: installation}})
	if err != nil {
		return receipt, err
	}
	if err := owner.AttachSessions(ctx, proto.Clone(installation).(*agentv1.ComputerSessionInstallation)); err != nil {
		return receipt, err
	}
	err = owner.PrepareActivation(ctx, proto.Clone(installation).(*agentv1.ComputerSessionInstallation), proto.Clone(receipt).(*agentv1.ComputerSessionReceipt))
	if err != nil {
		return receipt, err
	}
	if !receipt.GetActivated() {
		controls, err := owner.CurrentControls(ctx, proto.Clone(installation).(*agentv1.ComputerSessionInstallation), proto.Clone(receipt).(*agentv1.ComputerSessionReceipt))
		if err != nil {
			return receipt, err
		}
		target, actual := installation.GetEnvelope(), controls.GetEnvelope()
		if controls == nil || controls.GetCheckpointId() != installation.GetCapture().GetCheckpointId() || controls.GetDesiredVersion() != installation.GetDesiredVersion() || actual.GetComputerId() != target.GetComputerId() || actual.GetComputerInstanceId() != target.GetComputerInstanceId() || actual.GetWriterGeneration() != target.GetWriterGeneration() || actual.GetChannelCredential() != target.GetChannelCredential() || actual.GetOperationId() != target.GetOperationId() {
			return receipt, errors.New("current Computer controls changed physical installation")
		}
		receipt, err = controlAgentComputer(ctx, machine, &agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Controls{Controls: controls}})
		if err != nil {
			return receipt, err
		}
	}
	receipt, err = controlAgentComputer(ctx, machine, &agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Activate{Activate: installation}})
	if err != nil {
		return receipt, err
	}
	if err := owner.ReconcileControls(ctx, proto.Clone(installation).(*agentv1.ComputerSessionInstallation), proto.Clone(receipt).(*agentv1.ComputerSessionReceipt)); err != nil {
		return receipt, err
	}
	return receipt, nil
}
