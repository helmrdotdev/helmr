package control

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
			err:  errors.New("workspace.repository must be \"owner/repo\""),
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
			err:  errors.New("authorize github workspace repository: database unavailable"),
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
