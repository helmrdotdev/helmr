package guestd

import (
	"context"
	"errors"
	"io"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

// Cancellation acknowledges intent. The output stream publishes the outcome only
// after the command's process scope has been reaped; peers retain their authority.
func (r *computerOperationRegistry) cancelCommand(ctx context.Context, a *computerv0.ComputerCommandAuthority) error {
	entry, release, ok := r.acquireCommandInstance(a.GetComputerInstanceId(), a.GetComputerId(), a.GetChannelCredential())
	if !ok {
		return errors.New("command Instance is unavailable")
	}
	defer release()
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	if !r.currentMountLocked(entry, entry.computerInstanceID, a.GetComputerId(), a.GetChannelCredential()) || entry.writerGeneration != a.GetWriterGeneration() {
		return errors.New("command writer changed")
	}
	if err := validateComputerBasicExecClaim(ctx, &computerv0.ComputerBasicExecRequest{Envelope: a}, entry.authorityNow()); err != nil {
		return err
	}
	if command := entry.commands[a.GetOperationId()]; command != nil {
		if command.envelope.GetRequestFingerprint() != a.GetRequestFingerprint() {
			return errors.New("command fingerprint changed")
		}
		if command.cancel != nil {
			command.cancel()
		}
		return nil
	}
	if r.captureSealed() || entry.stopping {
		return errors.New("computer has sealed Command admission")
	}
	output, err := newCommandOutputSpool(diagnosticLimits{ChunkBytes: 1, BufferBytes: 1, BufferRecords: 1})
	if err != nil {
		return err
	}
	if err := validateComputerBasicExecClaim(ctx, &computerv0.ComputerBasicExecRequest{Envelope: a}, entry.authorityNow()); err != nil {
		output.close()
		return err
	}
	// A cancellation arriving before launch must prevent a delayed launch too.
	command := &computerBasicExec{envelope: proto.Clone(a).(*computerv0.ComputerCommandAuthority), done: make(chan struct{}), output: output, result: computerBasicCommandFailure(a.GetRequestFingerprint(), "computer_command_cancelled", context.Canceled)}
	output.finish(true)
	command.result.Stdout = output.boundaries["stdout"]
	command.result.Stderr = output.boundaries["stderr"]
	close(command.done)
	if entry.commands == nil {
		entry.commands = make(map[string]*computerBasicExec)
	}
	entry.commands[a.GetOperationId()] = command
	r.mu.Lock()
	cleanup := entry.cleanup
	entry.cleanup = func() {
		output.close()
		if cleanup != nil {
			cleanup()
		}
	}
	r.mu.Unlock()
	return nil
}

func handleComputerCommandCancelConnection(ctx context.Context, conn io.ReadWriter, r *computerOperationRegistry) error {
	var request computerv0.ComputerCommandCancelRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return err
	}
	response := &computerv0.ComputerCommandCancelResponse{}
	if err := r.cancelCommand(ctx, request.GetAuthority()); err != nil {
		response.Error = err.Error()
	} else {
		response.Accepted = true
	}
	return frameio.WriteProtoFrame(conn, response)
}
