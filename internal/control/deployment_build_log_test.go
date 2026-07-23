package control

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

type deploymentBuildLogStore struct {
	events []db.AppendDeploymentEventParams
}

func (store *deploymentBuildLogStore) AppendDeploymentEvent(
	_ context.Context,
	event db.AppendDeploymentEventParams,
) (db.AppendDeploymentEventRow, error) {
	store.events = append(store.events, event)
	return db.AppendDeploymentEventRow{}, nil
}
