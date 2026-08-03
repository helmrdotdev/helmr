package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

func TestAppendDeploymentBuildLogsPreservesRawStreamsInBoundedEvents(t *testing.T) {
	stdout := bytes.Repeat([]byte("x"), (256<<10)+3)
	stderr := []byte{0, 1, 2, '\n'}
	store := &deploymentBuildLogStore{}
	err := appendDeploymentBuildLogs(
		context.Background(),
		store,
		db.AppendDeploymentEventParams{}.OrgID,
		db.AppendDeploymentEventParams{}.ProjectID,
		db.AppendDeploymentEventParams{}.EnvironmentID,
		db.AppendDeploymentEventParams{}.DeploymentID,
		deployment.BuildLogs{
			ExitStatus:   19,
			StdoutBase64: base64.StdEncoding.EncodeToString(stdout),
			StderrBase64: base64.StdEncoding.EncodeToString(stderr),
			Truncated:    true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 4 {
		t.Fatalf("events = %d, want exit plus three log chunks", len(store.events))
	}
	var exit struct {
		ExitStatus int32 `json:"exitStatus"`
		Truncated  bool  `json:"truncated"`
	}
	if err := json.Unmarshal(store.events[0].Payload, &exit); err != nil {
		t.Fatal(err)
	}
	if store.events[0].Kind != "deployment.build.exit" ||
		exit.ExitStatus != 19 ||
		!exit.Truncated {
		t.Fatalf("exit event = %+v payload=%+v", store.events[0], exit)
	}
	streams := map[string][]byte{}
	for _, event := range store.events[1:] {
		if event.Kind != "deployment.build.log" ||
			event.RedactionClass != "sensitive" {
			t.Fatalf("log event = %+v", event)
		}
		var payload struct {
			ContentBase64 string `json:"contentBase64"`
			Offset        int    `json:"offset"`
			Stream        string `json:"stream"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		chunk, err := base64.StdEncoding.DecodeString(payload.ContentBase64)
		if err != nil {
			t.Fatal(err)
		}
		if payload.Offset != len(streams[payload.Stream]) {
			t.Fatalf("%s offset = %d, want %d", payload.Stream, payload.Offset, len(streams[payload.Stream]))
		}
		streams[payload.Stream] = append(streams[payload.Stream], chunk...)
	}
	if !bytes.Equal(streams["stdout"], stdout) ||
		!bytes.Equal(streams["stderr"], stderr) {
		t.Fatal("persisted build streams changed")
	}
}

func TestAppendDeploymentBuildLogsAcceptsMaximumSplitAcrossStreams(t *testing.T) {
	const totalBytes = 16 << 20
	stdout := bytes.Repeat([]byte("x"), (8<<20)+1)
	stderr := bytes.Repeat([]byte("y"), totalBytes-len(stdout))
	store := &deploymentBuildLogCountStore{}
	err := appendDeploymentBuildLogs(
		context.Background(),
		store,
		db.AppendDeploymentEventParams{}.OrgID,
		db.AppendDeploymentEventParams{}.ProjectID,
		db.AppendDeploymentEventParams{}.EnvironmentID,
		db.AppendDeploymentEventParams{}.DeploymentID,
		deployment.BuildLogs{
			StdoutBase64: base64.StdEncoding.EncodeToString(stdout),
			StderrBase64: base64.StdEncoding.EncodeToString(stderr),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if store.count != 66 {
		t.Fatalf("events = %d, want exit plus 65 log chunks", store.count)
	}
}

type deploymentBuildLogStore struct {
	events []db.AppendDeploymentEventParams
}

func (store *deploymentBuildLogStore) AppendDeploymentEvents(
	_ context.Context,
	batch db.AppendDeploymentEventsParams,
) (int64, error) {
	for index := range batch.Kinds {
		store.events = append(store.events, db.AppendDeploymentEventParams{
			OrgID: batch.OrgID, ProjectID: batch.ProjectID,
			EnvironmentID: batch.EnvironmentID, DeploymentID: batch.DeploymentID,
			Category: batch.Categories[index], Severity: batch.Severities[index],
			Source: batch.Sources[index], Kind: batch.Kinds[index],
			Message: batch.Messages[index], Payload: json.RawMessage(batch.Payloads[index]),
			RedactionClass: batch.RedactionClasses[index],
		})
	}
	return int64(len(batch.Kinds)), nil
}

type deploymentBuildLogCountStore struct {
	count int
}

func (store *deploymentBuildLogCountStore) AppendDeploymentEvents(
	_ context.Context,
	batch db.AppendDeploymentEventsParams,
) (int64, error) {
	store.count = len(batch.Kinds)
	return int64(store.count), nil
}
