package runprotocol

import (
	"fmt"
	"io"
	"strings"

	"github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/transport"
	"google.golang.org/protobuf/proto"
)

func WriteCheckpointPauseRequest(w io.Writer, request *runv0.CheckpointPauseRequest) error {
	if request == nil {
		return fmt.Errorf("checkpoint pause request is required")
	}
	body, err := proto.Marshal(request)
	if err != nil {
		return fmt.Errorf("marshal checkpoint pause request: %w", err)
	}
	if err := transport.WriteStreamFrameHeader(w, transport.StreamHeader{
		Type:         transport.StreamTypeCheckpointPauseRequest,
		RunWaitID:    request.RunWaitId,
		CheckpointID: request.CheckpointId,
	}, uint64(len(body))); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func WriteResumeDecision(w io.Writer, decision *runv0.ResumeDecision) error {
	if decision == nil {
		return fmt.Errorf("resume decision is required")
	}
	body, err := proto.Marshal(decision)
	if err != nil {
		return fmt.Errorf("marshal resume decision: %w", err)
	}
	if err := transport.WriteStreamFrameHeader(w, transport.StreamHeader{
		Type:      transport.StreamTypeResumeDecision,
		RunWaitID: decision.RunWaitId,
	}, uint64(len(body))); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func ReadCheckpointPauseRequest(header transport.StreamHeader, reader io.Reader, bodyLen uint64) (*runv0.CheckpointPauseRequest, error) {
	if header.Type != transport.StreamTypeCheckpointPauseRequest {
		return nil, fmt.Errorf("expected checkpoint pause request frame, got %q", header.Type)
	}
	var request runv0.CheckpointPauseRequest
	if err := readProtoStreamBody(reader, bodyLen, &request); err != nil {
		return nil, fmt.Errorf("read checkpoint pause request: %w", err)
	}
	if strings.TrimSpace(header.RunWaitID) != strings.TrimSpace(request.RunWaitId) ||
		strings.TrimSpace(header.CheckpointID) != strings.TrimSpace(request.CheckpointId) {
		return nil, fmt.Errorf("checkpoint pause request header mismatch: run_wait_id=%q/%q checkpoint_id=%q/%q", header.RunWaitID, request.RunWaitId, header.CheckpointID, request.CheckpointId)
	}
	return &request, nil
}

func ReadResumeDecision(header transport.StreamHeader, reader io.Reader, bodyLen uint64) (*runv0.ResumeDecision, error) {
	if header.Type != transport.StreamTypeResumeDecision {
		return nil, fmt.Errorf("expected resume decision frame, got %q", header.Type)
	}
	var decision runv0.ResumeDecision
	if err := readProtoStreamBody(reader, bodyLen, &decision); err != nil {
		return nil, fmt.Errorf("read resume decision: %w", err)
	}
	if strings.TrimSpace(header.RunWaitID) != strings.TrimSpace(decision.RunWaitId) {
		return nil, fmt.Errorf("resume decision header mismatch: run_wait_id=%q/%q", header.RunWaitID, decision.RunWaitId)
	}
	return &decision, nil
}

func readProtoStreamBody(reader io.Reader, bodyLen uint64, message proto.Message) error {
	if bodyLen == 0 {
		return fmt.Errorf("protobuf stream frame body is required")
	}
	if bodyLen > uint64(transport.MaxFrameBytes) {
		return fmt.Errorf("protobuf stream frame body length %d exceeds max %d", bodyLen, transport.MaxFrameBytes)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(reader, body); err != nil {
		return err
	}
	return proto.Unmarshal(body, message)
}
