package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/conversation"
)

func TestRuntimeContentBudgetRetriesAndSeparateResponse(t *testing.T) {
	f := newFixture(t)
	a := f.enqueue(t, "content")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	empty, _ := conversation.Content(json.RawMessage(`""`), true)
	raw, _ := json.Marshal(strings.Repeat("x", conversation.ContentBytes-len(empty)))
	operation := uuid.NewV7()
	var sequence int64
	for i := range 32 {
		id := uuid.NewV7()
		if i == 0 {
			id = operation
		}
		got, err := RuntimeOutput(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, id, raw)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			sequence = got
		}
	}
	if got, err := RuntimeOutput(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, operation, raw); err != nil || got != sequence {
		t.Fatalf("replay %d: %v", got, err)
	}
	if _, err := RuntimeOutput(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, uuid.NewV7(), json.RawMessage(`"x"`)); !errors.Is(err, conversation.ErrLimit) {
		t.Fatalf("budget exceeded: %v", err)
	}
	response := uuid.NewV7()
	if err := RuntimeRespond(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, response, raw); err != nil {
		t.Fatalf("separate response budget: %v", err)
	}
	if err := RuntimeRespond(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, response, raw); err != nil {
		t.Fatalf("response replay: %v", err)
	}
	if err := RuntimeRespond(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, response, json.RawMessage(`[]`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed replay: %v", err)
	}
	if err := RuntimeRespond(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, uuid.NewV7(), raw); !errors.Is(err, ErrResponseAlreadyStaged) {
		t.Fatalf("second response: %v", err)
	}
	var total int64
	if err := f.pool.QueryRow(t.Context(), `SELECT progress_bytes FROM turns WHERE id=$1`, a.TurnID).Scan(&total); err != nil || total != conversation.ProgressBytes {
		t.Fatalf("budget changed %d: %v", total, err)
	}
}

func TestRuntimeResponsePublishesOnlyWithOwnSave(t *testing.T) {
	f := newFixture(t)
	storage := newSaveStorageFixture(t, f)
	a := f.enqueue(t, "response")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	response := json.RawMessage(`[{"type":"text","text":"a\u0000b"},{"type":"json","value":false}]`)
	responseID := uuid.NewV7()
	if err := RuntimeRespond(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, responseID, response); err != nil {
		t.Fatal(err)
	}
	view, err := GetTurn(t.Context(), f.pool, f.caller(), f.env, f.session, a.TurnID)
	if err != nil || view.Response != nil {
		t.Fatalf("premature response %+v: %v", view, err)
	}
	if err := RuntimeCloseProcessing(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID); err != nil {
		t.Fatal(err)
	}
	outputID := uuid.NewV7()
	sequence, err := RuntimeOutput(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, outputID, json.RawMessage(`"drained\u0000"`))
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := RuntimeFinalize(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, json.RawMessage(`42`), "joined"); err != nil || outcome != nil {
		t.Fatalf("premature completion %s: %v", outcome, err)
	}
	if _, err := RuntimeOutput(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, uuid.NewV7(), json.RawMessage(`"late"`)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("late output %v", err)
	}
	page, err := ListEvents(t.Context(), f.pool, f.caller(), TurnListRequest{EnvironmentID: f.env, SessionID: f.session, After: sequence - 1, Limit: 100})
	if err != nil || len(page.Records) != 2 || page.Records[0].Kind != "turn.output" || page.Records[1].Kind != "turn.finalizing" || string(page.Records[0].Data) != `[{"text":"drained\u0000","type":"text"}]` {
		t.Fatalf("progress while saving %+v: %v", page, err)
	}
	view, err = GetTurn(t.Context(), f.pool, f.caller(), f.env, f.session, a.TurnID)
	if err != nil || view.Status != "finalizing" || view.Response != nil || view.Result != nil {
		t.Fatalf("unpublished while saving %+v: %v", view, err)
	}
	var save SaveRequest
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM computer_saves WHERE turn_id=$1`, a.TurnID).Scan(&save.ID); err != nil {
		t.Fatal(err)
	}
	root, _ := storage.cut(t, 8)
	if err := storage.publisher.Capture(t.Context(), storage.ref(save.ID), root, "cut"); err != nil {
		t.Fatal(err)
	}
	if err := storage.publish(t, save.ID, root); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, a.TurnID); err != nil {
		t.Fatal(err)
	}
	outcome, err := RuntimeFinalize(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, json.RawMessage(`42`), "joined")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Status   string
		Result   int
		Response json.RawMessage
	}
	if err := json.Unmarshal(outcome, &parsed); err != nil || parsed.Status != "completed" || parsed.Result != 42 {
		t.Fatalf("outcome %s %v", outcome, err)
	}
	want, _ := conversation.Content(response, false)
	if string(parsed.Response) != string(want) {
		t.Fatalf("response %s", parsed.Response)
	}
	if got, err := RuntimeOutput(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, outputID, json.RawMessage(`"drained\u0000"`)); err != nil || got != sequence {
		t.Fatalf("terminal output receipt %d %v", got, err)
	}
	if err := RuntimeRespond(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, responseID, response); err != nil {
		t.Fatalf("terminal response receipt %v", err)
	}
}
