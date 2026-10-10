package guestd

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

func TestAgentFrameEpochAndBounds(t *testing.T) {
	identity := &agentv1.SessionIdentity{SessionId: "session", ProcessEpoch: 2}
	event := &agentv1.ProgramEvent{Identity: identity, Event: &agentv1.ProgramEvent_Ready{Ready: &agentv1.SessionReady{}}}
	body, err := proto.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, len(body)+4)
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	if decoded, err := readAgentEvent(bytes.NewReader(frame), identity); err != nil || !proto.Equal(decoded, event) {
		t.Fatalf("decode = %v, %v", decoded, err)
	}
	if _, err := readAgentEvent(bytes.NewReader(frame), &agentv1.SessionIdentity{SessionId: "session", ProcessEpoch: 1}); err == nil {
		t.Fatal("stale epoch accepted")
	}
	if _, err := readAgentEvent(bytes.NewReader(frame[:len(frame)-1]), identity); err == nil {
		t.Fatal("truncated body accepted")
	}
	for _, size := range []uint32{0, maxAgentFrameBytes + 1} {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], size)
		if _, err := readAgentEvent(bytes.NewReader(header[:]), identity); err == nil {
			t.Fatal("invalid bound accepted")
		}
	}
	var output bytes.Buffer
	command := &agentv1.GuestCommand{Identity: identity, Command: &agentv1.GuestCommand_Resume{Resume: &agentv1.SessionResume{}}}
	if err := writeAgentCommand(&output, identity, command); err != nil {
		t.Fatal(err)
	}
	var prefix [4]byte
	if _, err := io.ReadFull(&output, prefix[:]); err != nil {
		t.Fatal(err)
	}
	if int(binary.BigEndian.Uint32(prefix[:])) != output.Len() {
		t.Fatal("frame length includes prefix")
	}
	decoded := new(agentv1.GuestCommand)
	if err := proto.Unmarshal(output.Bytes(), decoded); err != nil || !proto.Equal(decoded, command) {
		t.Fatalf("command = %v,%v", decoded, err)
	}
	if err := writeAgentCommand(&output, &agentv1.SessionIdentity{SessionId: "other", ProcessEpoch: 2}, command); err == nil {
		t.Fatal("foreign command accepted")
	}
}
