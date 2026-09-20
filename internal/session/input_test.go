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
		{name: "open", actor: db.Session{Status: "open", NextInputSequence: 2}, want: true},
		{name: "closing", actor: db.Session{Status: "closing", NextInputSequence: 2}, want: true},
		{name: "closed", actor: db.Session{Status: "closed"}},
		{name: "held", actor: db.Session{Status: "open", DispatchHoldID: pgvalue.UUID(uuid.NewV7())}},
		{name: "current Run", actor: db.Session{Status: "open", CurrentRunID: pgtype.UUID{Valid: true}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := CanStartContinuation(test.actor); got != test.want {
				t.Fatalf("CanStartContinuation() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestTurnResolutionProjectsExternalInput(t *testing.T) {
	turnID := uuid.NewV7()
	createdAt := time.Date(2026, 7, 23, 1, 2, 3, 456000000, time.UTC)
	resolution, err := TurnResolution(db.SessionTurn{
		ID: pgvalue.UUID(turnID), Sequence: 7, Data: []byte(`{"nested":{"ok":true}}`),
		CreatedAt: pgvalue.Timestamptz(createdAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(resolution, &got); err != nil {
		t.Fatal(err)
	}
	turn := got["turn"].(map[string]any)
	source := turn["source"].(map[string]any)
	if got["value"].(map[string]any)["nested"].(map[string]any)["ok"] != true ||
		turn["id"] != turnID.String() || turn["sequence"] != float64(7) ||
		turn["created_at"] != createdAt.Format(time.RFC3339Nano) || source["type"] != "external" {
		t.Fatalf("resolution = %s", resolution)
	}
	if _, exists := source["run_id"]; exists {
		t.Fatalf("external source exposed run_id: %s", resolution)
	}
}

func TestTurnResolutionProjectsRunSource(t *testing.T) {
	runID := uuid.NewV7()
	resolution, err := TurnResolution(db.SessionTurn{
		ID: pgvalue.UUID(uuid.NewV7()), Sequence: 1, Data: []byte(`null`),
		SourceRunID: pgvalue.UUID(runID),
		CreatedAt:   pgvalue.Timestamptz(time.Unix(1, 0).UTC()),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Turn struct {
			Source map[string]string `json:"source"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(resolution, &got); err != nil {
		t.Fatal(err)
	}
	if got.Turn.Source["type"] != "run" || got.Turn.Source["run_id"] != runID.String() {
		t.Fatalf("source = %+v", got.Turn.Source)
	}
}

func TestTurnResolutionRejectsInvalidTurnJSON(t *testing.T) {
	_, err := TurnResolution(db.SessionTurn{Data: []byte(`{"broken"`)})
	if err == nil {
		t.Fatal("invalid durable turn JSON was accepted")
	}
}
