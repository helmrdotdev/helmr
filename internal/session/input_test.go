package session

import (
	"encoding/json"
	"testing"
	"time"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCanStartContinuationIncludesClosingBacklog(t *testing.T) {
	for _, test := range []struct {
		name  string
		actor db.Session
		want  bool
	}{
		{name: "open", actor: db.Session{State: "open"}, want: true},
		{name: "closing", actor: db.Session{State: "closing"}, want: true},
		{name: "closed", actor: db.Session{State: "closed"}},
		{name: "manual cancellation", actor: db.Session{State: "open", ManualRunCancelled: true}},
		{name: "current Run", actor: db.Session{State: "open", CurrentRunID: pgtype.UUID{Valid: true}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := CanStartContinuation(test.actor); got != test.want {
				t.Fatalf("CanStartContinuation() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRecordResolutionProjectsExternalInput(t *testing.T) {
	recordID := uuid.NewV7()
	createdAt := time.Date(2026, 7, 23, 1, 2, 3, 456000000, time.UTC)
	resolution, err := RecordResolution(db.SessionRecord{
		ID: pgvalue.UUID(recordID), Sequence: 7, Data: []byte(`{"nested":{"ok":true}}`),
		SourceKind: pgvalue.Text("external"), CreatedAt: pgvalue.Timestamptz(createdAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(resolution, &got); err != nil {
		t.Fatal(err)
	}
	record := got["record"].(map[string]any)
	source := record["source"].(map[string]any)
	if got["value"].(map[string]any)["nested"].(map[string]any)["ok"] != true ||
		record["id"] != recordID.String() || record["sequence"] != float64(7) ||
		record["created_at"] != createdAt.Format(time.RFC3339Nano) || source["type"] != "external" {
		t.Fatalf("resolution = %s", resolution)
	}
	if _, exists := source["run_id"]; exists {
		t.Fatalf("external source exposed run_id: %s", resolution)
	}
}

func TestRecordResolutionProjectsRunSource(t *testing.T) {
	runID := uuid.NewV7()
	resolution, err := RecordResolution(db.SessionRecord{
		ID: pgvalue.UUID(uuid.NewV7()), Sequence: 1, Data: []byte(`null`),
		SourceKind: pgvalue.Text("run"), SourceRunID: pgvalue.UUID(runID),
		CreatedAt: pgvalue.Timestamptz(time.Unix(1, 0).UTC()),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Record struct {
			Source map[string]string `json:"source"`
		} `json:"record"`
	}
	if err := json.Unmarshal(resolution, &got); err != nil {
		t.Fatal(err)
	}
	if got.Record.Source["type"] != "run" || got.Record.Source["run_id"] != runID.String() {
		t.Fatalf("source = %+v", got.Record.Source)
	}
}

func TestRecordResolutionRejectsInvalidRecordJSON(t *testing.T) {
	_, err := RecordResolution(db.SessionRecord{Data: []byte(`{"broken"`)})
	if err == nil {
		t.Fatal("invalid durable record JSON was accepted")
	}
}
