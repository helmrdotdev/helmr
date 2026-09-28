package wire

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/helmrdotdev/helmr/internal/frameio"
)

type StreamType string

const (
	StreamTypeSessionStop             StreamType = "session-stop"
	StreamTypeRunImage                StreamType = "run-image"
	StreamTypeComputerArtifact        StreamType = "computer-artifact"
	StreamTypeCheckpointPauseRequest  StreamType = "checkpoint-pause-request"
	StreamTypeCheckpointPauseReady    StreamType = "checkpoint-pause-ready"
	StreamTypeResumeDecision          StreamType = "resume-decision"
	StreamTypeComputerMaterialize     StreamType = "computer-materialize"
	StreamTypeComputerRuntimePrepare  StreamType = "computer-runtime-prepare"
	StreamTypeProgramRun              StreamType = "program-run"
	StreamTypeComputerBasicExec       StreamType = "computer-basic-exec"
	StreamTypeComputerCommandCancel   StreamType = "computer-command-cancel"
	StreamTypeComputerCommandRelease  StreamType = "computer-command-release"
 StreamTypeComputerRunCleanup StreamType = "computer-run-cleanup"
	StreamTypeComputerAuthorityRenew  StreamType = "computer-authority-renew"
	StreamTypeProgramResumeGrant      StreamType = "program-resume-grant"
	StreamTypeComputerRestoreVerify   StreamType = "computer-restore-verify"
	StreamTypeComputerFreeze          StreamType = "computer-freeze"
	StreamTypeComputerRestoreInstall  StreamType = "computer-restore-install"
	StreamTypeComputerRestoreActivate StreamType = "computer-restore-activate"
)

type StreamHeader struct {
	Type               StreamType `json:"type"`
	RunID              string     `json:"run_id,omitempty"`
	TaskID             string     `json:"task_id,omitempty"`
	RunWaitID          string     `json:"run_wait_id,omitempty"`
	CheckpointID       string     `json:"checkpoint_id,omitempty"`
	ComputerID         string     `json:"computer_id,omitempty"`
	ComputerInstanceID string     `json:"computer_instance_id,omitempty"`
	OperationID        string     `json:"operation_id,omitempty"`
	BodyDigest         *string    `json:"body_digest,omitempty"`
	EntryCount         *int       `json:"entry_count,omitempty"`
}

func WriteStreamFrameHeader(w io.Writer, header StreamHeader, bodyLen uint64) error {
	headerBytes, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("marshal stream frame header: %w", err)
	}
	return frameio.WriteStreamFrameHeader(w, headerBytes, bodyLen)
}

func ReadStreamFrameHeader(r io.Reader) (StreamHeader, uint64, error) {
	headerBytes, bodyLen, err := frameio.ReadStreamFrameHeader(r)
	if err != nil {
		return StreamHeader{}, 0, err
	}
	var header StreamHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return StreamHeader{}, 0, fmt.Errorf("unmarshal stream frame header: %w", err)
	}
	return header, bodyLen, nil
}

func WriteFileFrame(w io.Writer, header StreamHeader, path string) error {
	digest, size, err := frameio.HashFile(path)
	if err != nil {
		return err
	}
	return WriteFileFrameWithMetadata(w, header, path, digest, size)
}

func WriteFileFrameWithMetadata(w io.Writer, header StreamHeader, path string, digest string, size int64) error {
	header.BodyDigest = &digest
	headerBytes, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("marshal file frame header: %w", err)
	}
	return frameio.WriteFileFrameWithMetadata(w, headerBytes, path, size)
}
