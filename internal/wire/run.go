package wire

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"google.golang.org/protobuf/proto"
)

func WriteCheckpointPauseRequest(w io.Writer, request *programv0.CheckpointPauseRequest) error {
	if request == nil {
		return fmt.Errorf("checkpoint pause request is required")
	}
	body, err := proto.Marshal(request)
	if err != nil {
		return fmt.Errorf("marshal checkpoint pause request: %w", err)
	}
	var frame bytes.Buffer
	if err := WriteStreamFrameHeader(&frame, StreamHeader{
		Type:         StreamTypeCheckpointPauseRequest,
		RunWaitID:    request.RunWaitId,
		CheckpointID: request.CheckpointId,
	}, uint64(len(body))); err != nil {
		return err
	}
	_, _ = frame.Write(body)
	_, err = w.Write(frame.Bytes())
	return err
}

func WriteCheckpointPauseReady(w io.Writer, runWaitID string, checkpointID string) error {
	return WriteStreamFrameHeader(w, StreamHeader{
		Type:         StreamTypeCheckpointPauseReady,
		RunWaitID:    runWaitID,
		CheckpointID: checkpointID,
	}, 0)
}

func WriteResumeDecision(w io.Writer, decision *programv0.ResumeDecision) error {
	if decision == nil {
		return fmt.Errorf("resume decision is required")
	}
	body, err := proto.Marshal(decision)
	if err != nil {
		return fmt.Errorf("marshal resume decision: %w", err)
	}
	var frame bytes.Buffer
	if err := WriteStreamFrameHeader(&frame, StreamHeader{
		Type:      StreamTypeResumeDecision,
		RunWaitID: decision.RunWaitId,
	}, uint64(len(body))); err != nil {
		return err
	}
	_, _ = frame.Write(body)
	_, err = w.Write(frame.Bytes())
	return err
}

func ReadCheckpointPauseRequest(header StreamHeader, reader io.Reader, bodyLen uint64) (*programv0.CheckpointPauseRequest, error) {
	if header.Type != StreamTypeCheckpointPauseRequest {
		return nil, fmt.Errorf("expected checkpoint pause request frame, got %q", header.Type)
	}
	var request programv0.CheckpointPauseRequest
	if err := readProtoStreamBody(reader, bodyLen, &request); err != nil {
		return nil, fmt.Errorf("read checkpoint pause request: %w", err)
	}
	if strings.TrimSpace(header.RunWaitID) != strings.TrimSpace(request.RunWaitId) ||
		strings.TrimSpace(header.CheckpointID) != strings.TrimSpace(request.CheckpointId) {
		return nil, fmt.Errorf("checkpoint pause request header mismatch: run_wait_id=%q/%q checkpoint_id=%q/%q", header.RunWaitID, request.RunWaitId, header.CheckpointID, request.CheckpointId)
	}
	return &request, nil
}

func ReadResumeDecision(header StreamHeader, reader io.Reader, bodyLen uint64) (*programv0.ResumeDecision, error) {
	if header.Type != StreamTypeResumeDecision {
		return nil, fmt.Errorf("expected resume decision frame, got %q", header.Type)
	}
	var decision programv0.ResumeDecision
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
	if bodyLen > uint64(frameio.MaxFrameBytes) {
		return fmt.Errorf("protobuf stream frame body length %d exceeds max %d", bodyLen, frameio.MaxFrameBytes)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(reader, body); err != nil {
		return err
	}
	return proto.Unmarshal(body, message)
}

// SecretEnvCollisionDiagnostic is fixed public text; never append underlying launch errors.
const SecretEnvCollisionDiagnostic = "workspace Secret env binding conflicts with image or execution env; remove the bound name from image ENV and exec env"

func WriteSessionStop(w io.Writer, stop *programv0.SessionStop) error {
	if stop == nil {
		return fmt.Errorf("session stop is required")
	}
	body, err := proto.Marshal(stop)
	if err != nil {
		return err
	}
	var frame bytes.Buffer
	if err := WriteStreamFrameHeader(&frame, StreamHeader{Type: StreamTypeSessionStop, RunID: stop.GetExecution().GetRunId()}, uint64(len(body))); err != nil {
		return err
	}
	_, _ = frame.Write(body)
	_, err = w.Write(frame.Bytes())
	return err
}
func ReadSessionStop(header StreamHeader, reader io.Reader, bodyLen uint64) (*programv0.SessionStop, error) {
	if header.Type != StreamTypeSessionStop {
		return nil, fmt.Errorf("expected Session stop")
	}
	var stop programv0.SessionStop
	if err := readProtoStreamBody(reader, bodyLen, &stop); err != nil {
		return nil, err
	}
	if header.RunID != stop.GetExecution().GetRunId() {
		return nil, fmt.Errorf("session stop Run identity mismatch")
	}
	return &stop, nil
}
