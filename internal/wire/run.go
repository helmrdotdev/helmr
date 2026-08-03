package wire

import (
	"fmt"
	"io"
	"strings"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/proto/run/v0"
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
	if err := WriteStreamFrameHeader(w, StreamHeader{
		Type:         StreamTypeCheckpointPauseRequest,
		RunWaitID:    request.RunWaitId,
		CheckpointID: request.CheckpointId,
	}, uint64(len(body))); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func WriteCheckpointPauseReady(w io.Writer, ready *runv0.CheckpointPauseReady) error {
	if ready == nil {
		return fmt.Errorf("checkpoint pause ready is required")
	}
	body, err := proto.Marshal(ready)
	if err != nil {
		return fmt.Errorf("marshal checkpoint pause ready: %w", err)
	}
	if err := WriteStreamFrameHeader(w, StreamHeader{
		Type:         StreamTypeCheckpointPauseReady,
		RunWaitID:    ready.RunWaitId,
		CheckpointID: ready.CheckpointId,
	}, uint64(len(body))); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func WriteActorTurnCommitPauseRequest(w io.Writer, request *runv0.ActorTurnCommitPauseRequest) error {
	if request == nil {
		return fmt.Errorf("actor turn commit pause request is required")
	}
	body, err := proto.Marshal(request)
	if err != nil {
		return fmt.Errorf("marshal actor turn commit pause request: %w", err)
	}
	if err := WriteStreamFrameHeader(w, StreamHeader{
		Type:  StreamTypeActorTurnCommitPause,
		RunID: request.RunId,
	}, uint64(len(body))); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func WriteActorTurnCommitPauseReady(w io.Writer, ready *runv0.ActorTurnCommitPauseReady) error {
	if ready == nil {
		return fmt.Errorf("actor turn commit pause ready is required")
	}
	body, err := proto.Marshal(ready)
	if err != nil {
		return fmt.Errorf("marshal actor turn commit pause ready: %w", err)
	}
	if err := WriteStreamFrameHeader(w, StreamHeader{
		Type:  StreamTypeActorTurnCommitReady,
		RunID: ready.RunId,
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
	if err := WriteStreamFrameHeader(w, StreamHeader{
		Type:      StreamTypeResumeDecision,
		RunWaitID: decision.RunWaitId,
	}, uint64(len(body))); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func ReadCheckpointPauseRequest(header StreamHeader, reader io.Reader, bodyLen uint64) (*runv0.CheckpointPauseRequest, error) {
	if header.Type != StreamTypeCheckpointPauseRequest {
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

func ReadActorTurnCommitPauseRequest(header StreamHeader, reader io.Reader, bodyLen uint64) (*runv0.ActorTurnCommitPauseRequest, error) {
	if header.Type != StreamTypeActorTurnCommitPause {
		return nil, fmt.Errorf("expected actor turn commit pause request frame, got %q", header.Type)
	}
	var request runv0.ActorTurnCommitPauseRequest
	if err := readProtoStreamBody(reader, bodyLen, &request); err != nil {
		return nil, fmt.Errorf("read actor turn commit pause request: %w", err)
	}
	if strings.TrimSpace(header.RunID) != strings.TrimSpace(request.RunId) {
		return nil, fmt.Errorf("actor turn commit pause request header mismatch: run_id=%q/%q", header.RunID, request.RunId)
	}
	return &request, nil
}

func ReadActorTurnCommitPauseReady(header StreamHeader, reader io.Reader, bodyLen uint64) (*runv0.ActorTurnCommitPauseReady, error) {
	if header.Type != StreamTypeActorTurnCommitReady {
		return nil, fmt.Errorf("expected actor turn commit pause ready frame, got %q", header.Type)
	}
	var ready runv0.ActorTurnCommitPauseReady
	if err := readProtoStreamBody(reader, bodyLen, &ready); err != nil {
		return nil, fmt.Errorf("read actor turn commit pause ready: %w", err)
	}
	if strings.TrimSpace(header.RunID) != strings.TrimSpace(ready.RunId) {
		return nil, fmt.Errorf("actor turn commit pause ready header mismatch: run_id=%q/%q", header.RunID, ready.RunId)
	}
	return &ready, nil
}

func ReadResumeDecision(header StreamHeader, reader io.Reader, bodyLen uint64) (*runv0.ResumeDecision, error) {
	if header.Type != StreamTypeResumeDecision {
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
	if bodyLen > uint64(frameio.MaxFrameBytes) {
		return fmt.Errorf("protobuf stream frame body length %d exceeds max %d", bodyLen, frameio.MaxFrameBytes)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(reader, body); err != nil {
		return err
	}
	return proto.Unmarshal(body, message)
}
