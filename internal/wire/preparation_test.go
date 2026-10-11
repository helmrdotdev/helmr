package wire

import (
	"bytes"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"testing"
)

func TestPreparationRejectsOversizedSecretAggregateBeforeSending(t *testing.T) {
	request := &computerv0.PreparationControlRequest{Start: &computerv0.PreparationStart{LogLimits: &computerv0.PreparationLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 4}, ComputerDefinitionId: "repo"}}
	// Reusing the value keeps the test small; each placement counts in the frame.
	value := bytes.Repeat([]byte{1}, 80<<10)
	for range 16 {
		request.Start.Secrets = append(request.Start.Secrets, &computerv0.ComputerSecretDelivery{PlacementKind: "env", PlacementTarget: "TOKEN", Value: value})
	}
	if err := ValidatePreparationControl(request); err == nil {
		t.Fatal("oversized aggregate accepted")
	}
	request.Start.Secrets = request.Start.Secrets[:1]
	if err := ValidatePreparationControl(request); err != nil {
		t.Fatal(err)
	}
}

func TestPreparationSecretEnvironmentEntryIncludesNameAndTerminators(t *testing.T) {
	secret := &computerv0.ComputerSecretDelivery{PlacementKind: "env", PlacementTarget: "TOKEN", Value: bytes.Repeat([]byte{1}, (128<<10)-len("TOKEN")-2)}
	request := &computerv0.PreparationControlRequest{Start: &computerv0.PreparationStart{LogLimits: &computerv0.PreparationLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 4}, Secrets: []*computerv0.ComputerSecretDelivery{secret}}}
	if err := ValidatePreparationControl(request); err != nil {
		t.Fatal(err)
	}
	secret.Value = append(secret.Value, 1)
	if err := ValidatePreparationControl(request); err == nil {
		t.Fatal("environment entry over Linux bound accepted")
	}
}

func TestPreparationFileSecretDoesNotConsumeEnvironmentEntryLimit(t *testing.T) {
	request := &computerv0.PreparationControlRequest{Start: &computerv0.PreparationStart{
		LogLimits: &computerv0.PreparationLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 4},
		Secrets:   []*computerv0.ComputerSecretDelivery{{PlacementKind: "file", PlacementTarget: "/etc/service/token", Value: bytes.Repeat([]byte{1}, 256<<10)}},
	}}
	if err := ValidatePreparationControl(request); err != nil {
		t.Fatal(err)
	}
	request.Start.Secrets[0].Value = bytes.Repeat([]byte{1}, PreparationControlFrameBytes)
	if err := ValidatePreparationControl(request); err == nil {
		t.Fatal("file bypassed aggregate frame limit")
	}
	request.Start.Secrets[0].Value = nil
	request.Start.Secrets[0].PlacementKind = "unknown"
	if err := ValidatePreparationControl(request); err == nil {
		t.Fatal("unknown placement accepted")
	}
}
