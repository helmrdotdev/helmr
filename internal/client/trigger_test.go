package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

const testTriggerID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36"

func TestTriggerReadsUseAPIKeyRoutes(t *testing.T) {
	testTriggerReads(
		t,
		false,
		"/v1/triggers",
		EnvironmentScopeOptions{},
	)
}

func TestTriggerReadsUseSessionRoutes(t *testing.T) {
	testTriggerReads(
		t,
		true,
		"/api/projects/project-1/environments/env-1/triggers",
		EnvironmentScopeOptions{ProjectID: "project-1", EnvironmentID: "env-1"},
	)
}

func testTriggerReads(
	t *testing.T,
	session bool,
	collectionPath string,
	scope EnvironmentScopeOptions,
) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case collectionPath:
			if r.Method != http.MethodGet ||
				r.URL.Query().Get("cursor") != "cursor-current" ||
				r.URL.Query().Get("limit") != "25" {
				t.Fatalf("%s %s", r.Method, r.URL.RequestURI())
			}
			_ = json.NewEncoder(w).Encode(api.ListTriggersResponse{
				Triggers:   []api.TriggerResponse{{ID: testTriggerID, Agent: "nightly", TriggerKey: "daily"}},
				NextCursor: "cursor-next",
			})
		case collectionPath + "/" + testTriggerID:
			if r.Method != http.MethodGet {
				t.Fatalf("%s %s", r.Method, r.URL.RequestURI())
			}
			_ = json.NewEncoder(w).Encode(api.TriggerResponse{
				ID: testTriggerID, Agent: "nightly", TriggerKey: "daily",
			})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	options := []Option{WithHTTPClient(server.Client())}
	if session {
		options = append(options, WithSessionScopedRoutes())
	}
	controlPlane, err := New(server.URL, options...)
	if err != nil {
		t.Fatal(err)
	}
	page, err := controlPlane.ListTriggers(context.Background(), ListTriggersOptions{
		Cursor: "cursor-current", Limit: 25, EnvironmentScopeOptions: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Triggers) != 1 || page.NextCursor != "cursor-next" {
		t.Fatalf("page = %+v", page)
	}
	trigger, err := controlPlane.GetTrigger(context.Background(), testTriggerID, scope)
	if err != nil {
		t.Fatal(err)
	}
	if trigger.ID != testTriggerID || trigger.Agent != "nightly" {
		t.Fatalf("trigger = %+v", trigger)
	}
}

func TestTriggerReadsValidateInput(t *testing.T) {
	transportCalls := 0
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		transportCalls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	controlPlane, err := New("https://control.example", WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		limit     int32
		triggerID string
		wantError string
	}{
		{name: "valid limit", limit: 25},
		{name: "oversized limit", limit: 101, wantError: "trigger list limit"},
		{name: "valid ID", triggerID: testTriggerID},
		{name: "invalid ID", triggerID: "trigger", wantError: "invalid trigger ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transportCalls = 0
			var err error
			if tc.triggerID == "" {
				_, err = controlPlane.ListTriggers(t.Context(), ListTriggersOptions{Limit: tc.limit})
			} else {
				_, err = controlPlane.GetTrigger(t.Context(), tc.triggerID, EnvironmentScopeOptions{})
			}
			if tc.wantError == "" {
				if err != nil || transportCalls != 1 {
					t.Fatalf("valid input: transport calls=%d err=%v", transportCalls, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) || transportCalls != 0 {
				t.Fatalf("want %q before transport, got calls=%d err=%v", tc.wantError, transportCalls, err)
			}
		})
	}
}
