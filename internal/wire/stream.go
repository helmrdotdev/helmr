package wire

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/helmrdotdev/helmr/internal/frameio"
)

type StreamType string

const (
	StreamTypePreparationControl     StreamType = "preparation-control"
	StreamTypeAgentComputer          StreamType = "agent-computer"
	StreamTypeAgentSession           StreamType = "agent-session"
	StreamTypeComputerMaterialize    StreamType = "computer-materialize"
	StreamTypeComputerRuntimePrepare StreamType = "computer-runtime-prepare"
	StreamTypeComputerBasicExec      StreamType = "computer-basic-exec"
	StreamTypeComputerCommandCancel  StreamType = "computer-command-cancel"
	StreamTypeComputerCommandRelease StreamType = "computer-command-release"
	StreamTypeComputerFlush          StreamType = "computer-flush"
)

type StreamHeader struct {
	Type               StreamType `json:"type"`
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
