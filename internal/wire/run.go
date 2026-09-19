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

func WriteTurnSettlePauseRequest(w io.Writer, request *programv0.TurnSettlePauseRequest) error {
	if request == nil {
		return fmt.Errorf("actor turn commit pause request is required")
	}
	body, err := proto.Marshal(request)
	if err != nil {
		return fmt.Errorf("marshal actor turn commit pause request: %w", err)
	}
	var frame bytes.Buffer
	if err := WriteStreamFrameHeader(&frame, StreamHeader{
		Type:  StreamTypeTurnSettlePause,
		RunID: request.RunId,
	}, uint64(len(body))); err != nil {
		return err
	}
	_, _ = frame.Write(body)
	_, err = w.Write(frame.Bytes())
	return err
}

func WriteTurnSettlePauseReady(w io.Writer, ready *programv0.TurnSettlePauseReady) error {
	if ready == nil {
		return fmt.Errorf("actor turn commit pause ready is required")
	}
	body, err := proto.Marshal(ready)
	if err != nil {
		return fmt.Errorf("marshal actor turn commit pause ready: %w", err)
	}
	var frame bytes.Buffer
	if err := WriteStreamFrameHeader(&frame, StreamHeader{
		Type:  StreamTypeTurnSettleReady,
		RunID: ready.RunId,
	}, uint64(len(body))); err != nil {
		return err
	}
	_, _ = frame.Write(body)
	_, err = w.Write(frame.Bytes())
	return err
}

func WriteTurnSettleApplied(w io.Writer, applied *programv0.TurnSettleApplied) error {
	if applied == nil {
		return fmt.Errorf("actor turn commit applied proof is required")
	}
	body, err := proto.Marshal(applied)
	if err != nil {
		return fmt.Errorf("marshal actor turn commit applied proof: %w", err)
	}
	var frame bytes.Buffer
	if err := WriteStreamFrameHeader(&frame, StreamHeader{
		Type: StreamTypeTurnSettleApplied, RunID: applied.RunId,
	}, uint64(len(body))); err != nil {
		return err
	}
	_, _ = frame.Write(body)
	_, err = w.Write(frame.Bytes())
	return err
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

func ReadTurnSettlePauseRequest(header StreamHeader, reader io.Reader, bodyLen uint64) (*programv0.TurnSettlePauseRequest, error) {
	if header.Type != StreamTypeTurnSettlePause {
		return nil, fmt.Errorf("expected actor turn commit pause request frame, got %q", header.Type)
	}
	var request programv0.TurnSettlePauseRequest
	if err := readProtoStreamBody(reader, bodyLen, &request); err != nil {
		return nil, fmt.Errorf("read actor turn commit pause request: %w", err)
	}
	if strings.TrimSpace(header.RunID) != strings.TrimSpace(request.RunId) {
		return nil, fmt.Errorf("actor turn commit pause request header mismatch: run_id=%q/%q", header.RunID, request.RunId)
	}
	return &request, nil
}

func ReadTurnSettlePauseReady(header StreamHeader, reader io.Reader, bodyLen uint64) (*programv0.TurnSettlePauseReady, error) {
	if header.Type != StreamTypeTurnSettleReady {
		return nil, fmt.Errorf("expected actor turn commit pause ready frame, got %q", header.Type)
	}
	var ready programv0.TurnSettlePauseReady
	if err := readProtoStreamBody(reader, bodyLen, &ready); err != nil {
		return nil, fmt.Errorf("read actor turn commit pause ready: %w", err)
	}
	if strings.TrimSpace(header.RunID) != strings.TrimSpace(ready.RunId) {
		return nil, fmt.Errorf("actor turn commit pause ready header mismatch: run_id=%q/%q", header.RunID, ready.RunId)
	}
	return &ready, nil
}

func ReadTurnSettleApplied(header StreamHeader, reader io.Reader, bodyLen uint64) (*programv0.TurnSettleApplied, error) {
	if header.Type != StreamTypeTurnSettleApplied {
		return nil, fmt.Errorf("expected actor turn commit applied frame, got %q", header.Type)
	}
	var applied programv0.TurnSettleApplied
	if err := readProtoStreamBody(reader, bodyLen, &applied); err != nil {
		return nil, fmt.Errorf("read actor turn commit applied proof: %w", err)
	}
	if strings.TrimSpace(header.RunID) != strings.TrimSpace(applied.RunId) {
		return nil, fmt.Errorf("actor turn commit applied header mismatch: run_id=%q/%q", header.RunID, applied.RunId)
	}
	return &applied, nil
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
		return fmt.Errorf("Session stop is required")
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
		return nil, fmt.Errorf("Session stop Run identity mismatch")
	}
	return &stop, nil
}
