package guestd

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

const maxAgentFrameBytes = 16 * 1024 * 1024

func writeAgentCommand(writer io.Writer, identity *agentv1.SessionIdentity, command *agentv1.GuestCommand) error {
	if !proto.Equal(command.GetIdentity(), identity) {
		return errors.New("command belongs to another Session process")
	}
	body, err := proto.Marshal(command)
	if err != nil {
		return err
	}
	if len(body) == 0 || len(body) > maxAgentFrameBytes {
		return errors.New("invalid Agent command length")
	}
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	_, err = io.Copy(writer, bytes.NewReader(frame))
	return err
}

func readAgentEvent(reader io.Reader, identity *agentv1.SessionIdentity) (*agentv1.ProgramEvent, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > maxAgentFrameBytes {
		return nil, errors.New("invalid Agent event length")
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	event := new(agentv1.ProgramEvent)
	if err := proto.Unmarshal(body, event); err != nil {
		return nil, err
	}
	if !proto.Equal(event.GetIdentity(), identity) {
		return nil, errors.New("event belongs to another Session process")
	}
	return event, nil
}
