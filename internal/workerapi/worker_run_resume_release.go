package workerapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func (request *RunResumeReleaseRequest) UnmarshalJSON(raw []byte) error {
	var envelope struct {
		Lease                json.RawMessage `json:"lease"`
		RunWaitID            string          `json:"run_wait_id"`
		CheckpointID         string          `json:"checkpoint_id"`
		ResumeAttachID       string          `json:"resume_attach_id"`
		ResumeRequestVersion int64           `json:"resume_request_version"`
	}
	if err := decodeClosedWorkerRunResumeReleaseJSON(raw, &envelope); err != nil {
		return fmt.Errorf("decode run resume release request: %w", err)
	}
	if len(envelope.Lease) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Lease), []byte("null")) {
		return errors.New("run resume release lease is required")
	}
	var lease RunLeaseFence
	if err := decodeClosedWorkerRunResumeReleaseJSON(envelope.Lease, &lease); err != nil {
		return fmt.Errorf("decode run resume release lease: %w", err)
	}
	*request = RunResumeReleaseRequest{
		Lease:                lease,
		RunWaitID:            envelope.RunWaitID,
		CheckpointID:         envelope.CheckpointID,
		ResumeAttachID:       envelope.ResumeAttachID,
		ResumeRequestVersion: envelope.ResumeRequestVersion,
	}
	return nil
}

func decodeClosedWorkerRunResumeReleaseJSON(raw []byte, value any) error {
	if _, err := jsoncanon.Transform(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}
