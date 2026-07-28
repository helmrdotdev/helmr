package wire

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/helmrdotdev/helmr/internal/frameio"
)

type StreamType string

const (
	StreamTypeBuild                      StreamType = "build"
	StreamTypeRunImage                   StreamType = "run-image"
	StreamTypeWorkspaceArtifact          StreamType = "workspace-artifact"
	StreamTypeCheckpointPauseRequest     StreamType = "checkpoint-pause-request"
	StreamTypeCheckpointPauseReady       StreamType = "checkpoint-pause-ready"
	StreamTypeActorTurnCommitPause       StreamType = "actor-turn-commit-pause"
	StreamTypeActorTurnCommitReady       StreamType = "actor-turn-commit-ready"
	StreamTypeResumeDecision             StreamType = "resume-decision"
	StreamTypeWorkspaceMaterialize       StreamType = "workspace-materialize"
	StreamTypeWorkspaceRuntimePrepare    StreamType = "workspace-runtime-prepare"
	StreamTypeProgramRun                 StreamType = "program-run"
	StreamTypeWorkspaceBasicExec         StreamType = "workspace-basic-exec"
	StreamTypeWorkspaceStop              StreamType = "workspace-stop"
	StreamTypeWorkspaceAuthorityRenew    StreamType = "workspace-authority-renew"
	StreamTypeProgramResumeGrant         StreamType = "program-resume-grant"
	StreamTypeProgramRestoreVerify       StreamType = "program-restore-verify"
	StreamTypeWorkspaceFinalizationBegin StreamType = "workspace-finalization-begin"
	StreamTypeWorkspaceCapture           StreamType = "workspace-capture"
	StreamTypeWorkspaceReset             StreamType = "workspace-reset"
)

type StreamHeader struct {
	Type             StreamType `json:"type"`
	RunID            string     `json:"run_id,omitempty"`
	TaskID           string     `json:"task_id,omitempty"`
	RunWaitID        string     `json:"run_wait_id,omitempty"`
	CheckpointID     string     `json:"checkpoint_id,omitempty"`
	WorkspaceID      string     `json:"workspace_id,omitempty"`
	WorkspaceMountID string     `json:"workspace_mount_id,omitempty"`
	OperationID      string     `json:"operation_id,omitempty"`
	BodyDigest       *string    `json:"body_digest,omitempty"`
	EntryCount       *int       `json:"entry_count,omitempty"`
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
