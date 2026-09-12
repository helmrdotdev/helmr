package controlplane

import (
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestParseSessionListQueryStatusFilter(t *testing.T) {
	query, err := parseSessionListQuery("status=failed&status=open,closed&status=open", "project", "environment")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(query.statuses, []api.SessionStatus{
		api.SessionStatusClosed, api.SessionStatusFailed, api.SessionStatusOpen,
	}) {
		t.Fatalf("statuses = %v", query.statuses)
	}
	if !slices.Equal(sessionStatusStates(query.statuses), []string{"closed", "failed", "open", "closing"}) {
		t.Fatalf("states = %v", sessionStatusStates(query.statuses))
	}
	unfiltered, err := parseSessionListQuery("", "project", "environment")
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered.statuses) != 0 || len(sessionStatusStates(unfiltered.statuses)) != 0 {
		t.Fatalf("unfiltered query = %+v", unfiltered)
	}
	for _, rawQuery := range []string{
		"status=",
		"status=closing",
		"status=running",
		"status=open,",
		"status=OPEN",
		"actor_id=operator.v1&key=thread%3A1&status=open",
	} {
		if _, err := parseSessionListQuery(rawQuery, "project", "environment"); err == nil {
			t.Fatalf("%q was accepted", rawQuery)
		}
	}
}

func TestParseSessionListQueryKeepsExactKeyLookup(t *testing.T) {
	query, err := parseSessionListQuery("actor_id=operator.v1&key=thread%3A1", "project", "environment")
	if err != nil {
		t.Fatal(err)
	}
	if !query.exactKeyLookup || query.actorID != "operator.v1" || query.key != "thread:1" || len(query.statuses) != 0 {
		t.Fatalf("query = %+v", query)
	}
	_, err = parseSessionListQuery("actor_id=operator.v1&key=thread%3A1&limit=1", "project", "environment")
	if err == nil || !strings.Contains(err.Error(), "cursor, limit and status are not allowed") {
		t.Fatalf("limit with exact lookup error = %v", err)
	}
}

func TestSessionListCursorIsBoundToStatusFilter(t *testing.T) {
	createdAt := time.Date(2026, 7, 24, 12, 0, 0, 123, time.UTC)
	sessionID := uuid.NewV7().String()
	statuses := []api.SessionStatus{api.SessionStatusFailed, api.SessionStatusOpen}
	raw, err := encodeSessionListCursor(sessionListCursor{
		ProjectID: "project", EnvironmentID: "environment",
		Statuses:  sessionStatusStrings(statuses),
		CreatedAt: createdAt.Format(time.RFC3339Nano), SessionID: sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	query, err := parseSessionListQuery("status=open&status=failed&cursor="+raw, "project", "environment")
	if err != nil {
		t.Fatal(err)
	}
	if query.cursor == nil || query.cursor.SessionID != sessionID ||
		!slices.Equal(query.cursor.Statuses, []string{"failed", "open"}) {
		t.Fatalf("cursor = %+v", query.cursor)
	}
	for name, rawQuery := range map[string]string{
		"missing filter":   "cursor=" + raw,
		"narrower filter":  "status=open&cursor=" + raw,
		"different filter": "status=closed&status=cancelled&cursor=" + raw,
	} {
		if _, err := parseSessionListQuery(rawQuery, "project", "environment"); err == nil ||
			!strings.Contains(err.Error(), "does not match the status filter") {
			t.Fatalf("%s: error = %v", name, err)
		}
	}

	unfiltered, err := encodeSessionListCursor(sessionListCursor{
		ProjectID: "project", EnvironmentID: "environment",
		CreatedAt: createdAt.Format(time.RFC3339Nano), SessionID: sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseSessionListQuery("cursor="+unfiltered, "project", "environment"); err != nil {
		t.Fatalf("unfiltered cursor was rejected: %v", err)
	}
	if _, err := parseSessionListQuery("status=open&cursor="+unfiltered, "project", "environment"); err == nil {
		t.Fatal("unfiltered cursor was accepted with a status filter")
	}
}
