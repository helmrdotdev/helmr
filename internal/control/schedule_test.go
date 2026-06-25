package control

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteCreateScheduleErrorClassifiesFailures(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	tests := []struct {
		name string
		err  error
		want int
	}{
		{
			name: "configuration",
			err:  errors.New("secret store is not configured"),
			want: http.StatusServiceUnavailable,
		},
		{
			name: "client",
			err:  errors.New("task must match a deployed task id"),
			want: http.StatusBadRequest,
		},
		{
			name: "missing schedule key",
			err:  errors.New("deduplication_key is required"),
			want: http.StatusBadRequest,
		},
		{
			name: "undeclared queue",
			err:  errors.New(`queue "schedule-e2e" is not declared in the selected deployment`),
			want: http.StatusBadRequest,
		},
		{
			name: "deployment selection",
			err:  runDeploymentSelectionErrorf("deployment_id 00000000-0000-0000-0000-000000000001 was not found in this environment"),
			want: http.StatusBadRequest,
		},
		{
			name: "unexpected internal",
			err:  errors.New("schedule store unavailable"),
			want: http.StatusInternalServerError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			server.writeCreateScheduleError(rec, tt.err)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestCreateScheduleRejectsDeploymentSelection(t *testing.T) {
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: &fakeStore{}, Auth: fakeAuth{}})
	body := []byte(`{
		"deduplication_key":"inspect-main",
		"task":"deploy",
		"cron":"0 * * * *",
		"options":{"deployment_id":"00000000-0000-0000-0000-000000000001"}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/schedules", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "deployment_id is not accepted for scheduled session starts") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
