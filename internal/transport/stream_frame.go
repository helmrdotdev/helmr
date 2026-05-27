package transport

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

var streamFrameMagic = [4]byte{'H', 'M', 'S', '1'}

type StreamType string

const (
	StreamTypeCatalogDeployment    StreamType = "catalog-deployment"
	StreamTypeCompileTaskBundle    StreamType = "compile-task-bundle"
	StreamTypeRunImage             StreamType = "run-image"
	StreamTypeDeploymentSource     StreamType = "deployment-source"
	StreamTypeWorkspaceArtifact    StreamType = "workspace-artifact"
	StreamTypeCheckpointPauseReady StreamType = "checkpoint-pause-ready"
)

type StreamHeader struct {
	Type         StreamType `json:"type"`
	RunID        string     `json:"run_id,omitempty"`
	TaskID       string     `json:"task_id,omitempty"`
	WaitpointID  string     `json:"waitpoint_id,omitempty"`
	CheckpointID string     `json:"checkpoint_id,omitempty"`
	BodyDigest   *string    `json:"body_digest,omitempty"`
}

func WriteStreamFrameHeader(w io.Writer, header StreamHeader, bodyLen uint64) error {
	headerBytes, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("marshal transport stream frame header: %w", err)
	}
	if len(headerBytes) > MaxFrameBytes {
		return fmt.Errorf("transport stream frame header length %d exceeds max %d", len(headerBytes), MaxFrameBytes)
	}
	totalLen := uint64(len(headerBytes)) + bodyLen
	if totalLen > uint64(^uint32(0)) {
		return fmt.Errorf("transport stream frame length %d exceeds max %d", totalLen, uint64(^uint32(0)))
	}
	var prefix [12]byte
	copy(prefix[:4], streamFrameMagic[:])
	binary.BigEndian.PutUint32(prefix[4:8], uint32(totalLen))
	binary.BigEndian.PutUint32(prefix[8:], uint32(len(headerBytes)))
	if _, err := w.Write(prefix[:]); err != nil {
		return err
	}
	_, err = w.Write(headerBytes)
	return err
}

func IsStreamFramePrefix(prefix []byte) bool {
	return len(prefix) >= len(streamFrameMagic) &&
		prefix[0] == streamFrameMagic[0] &&
		prefix[1] == streamFrameMagic[1] &&
		prefix[2] == streamFrameMagic[2] &&
		prefix[3] == streamFrameMagic[3]
}

func ReadStreamFrameHeader(r io.Reader) (StreamHeader, uint64, error) {
	var prefix [12]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return StreamHeader{}, 0, err
	}
	if !IsStreamFramePrefix(prefix[:4]) {
		return StreamHeader{}, 0, fmt.Errorf("transport stream frame magic mismatch")
	}
	totalLen := binary.BigEndian.Uint32(prefix[4:8])
	headerLen := binary.BigEndian.Uint32(prefix[8:])
	if headerLen > totalLen {
		return StreamHeader{}, 0, fmt.Errorf("transport stream frame header length %d exceeds frame length %d", headerLen, totalLen)
	}
	if headerLen > MaxFrameBytes {
		return StreamHeader{}, 0, fmt.Errorf("transport stream frame header length %d exceeds max %d", headerLen, MaxFrameBytes)
	}
	headerBytes := make([]byte, headerLen)
	if _, err := io.ReadFull(r, headerBytes); err != nil {
		return StreamHeader{}, 0, err
	}
	var header StreamHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return StreamHeader{}, 0, fmt.Errorf("unmarshal transport stream frame header: %w", err)
	}
	return header, uint64(totalLen - headerLen), nil
}
