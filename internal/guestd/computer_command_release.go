package guestd

import (
	"errors"
	"io"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
)

// Caller holds the entry lifecycle lock, including capture/restore barriers.
func (entry *computerMountEntry) hasUnreleasedCommands() bool {
	for _, command := range entry.commands {
		if !command.acknowledged {
			return true
		}
	}
	return false
}

// Release follows durable Control Plane outcome acknowledgement. A tombstone
// retains duplicate-launch protection for the remainder of this Instance.
func (r *computerOperationRegistry) releaseCommand(a *computerv0.ComputerCommandAuthority) error {
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
	command := entry.commands[a.GetOperationId()]
	if command == nil || command.envelope.GetRequestFingerprint() != a.GetRequestFingerprint() {
		return errors.New("command receipt is unavailable")
	}
	if command.acknowledged {
		return nil
	}
	select {
	case <-command.done:
	default:
		return errors.New("command is still running")
	}
	if command.result == nil || command.result.GetOutcome() == "computer_command_scope_termination_failed" || command.result.GetOutcome() == "computer_command_result_uncertain" {
		return errors.New("command process cleanup is unresolved")
	}
	command.output.close()
	command.acknowledged = true
	return nil
}

func handleComputerCommandReleaseConnection(conn io.ReadWriter, r *computerOperationRegistry) error {
	var request computerv0.ComputerCommandReleaseRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return err
	}
	response := &computerv0.ComputerCommandReleaseResponse{}
	if err := r.releaseCommand(request.GetAuthority()); err != nil {
		response.Error = err.Error()
	} else {
		response.Released = true
	}
	return frameio.WriteProtoFrame(conn, response)
}
