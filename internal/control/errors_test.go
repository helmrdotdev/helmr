package control

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestErrorStatus(t *testing.T) {
	baseErr := errors.New("boom")
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "bad request", err: badRequest(baseErr), want: http.StatusBadRequest},
		{name: "unauthorized", err: unauthorized(baseErr), want: http.StatusUnauthorized},
		{name: "forbidden", err: forbidden(baseErr), want: http.StatusForbidden},
		{name: "not found", err: notFound(baseErr), want: http.StatusNotFound},
		{name: "conflict", err: conflict(baseErr), want: http.StatusConflict},
		{name: "too large", err: tooLarge(baseErr), want: http.StatusRequestEntityTooLarge},
		{name: "bad gateway", err: badGateway(baseErr), want: http.StatusBadGateway},
		{name: "unavailable", err: unavailable(baseErr), want: http.StatusServiceUnavailable},
		{name: "unclassified", err: baseErr, want: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errorStatus(tt.err); got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
			if got := tt.err.Error(); got != baseErr.Error() {
				t.Fatalf("error = %q, want %q", got, baseErr.Error())
			}
			if !errors.Is(tt.err, baseErr) {
				t.Fatalf("errors.Is(%v, baseErr) = false", tt.err)
			}
		})
	}
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()

	writeError(rec, conflict(errors.New("run already exists")))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
	if got := rec.Header().Get("content-type"); got != "application/json" {
		t.Fatalf("content-type = %q", got)
	}
	if got, want := rec.Body.String(), "{\"error\":\"run already exists\"}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestWriteAuthErrorKeepsErrorKind(t *testing.T) {
	rec := httptest.NewRecorder()

	writeAuthError(rec, http.StatusBadRequest, errInvalidOrExpiredToken)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if got, want := rec.Body.String(), "{\"error\":\"token is invalid or expired\",\"error_kind\":\"invalid_token\"}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestIsNoRows(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "pgx", err: pgx.ErrNoRows, want: true},
		{name: "wrapped pgx", err: errors.Join(errors.New("load run"), pgx.ErrNoRows), want: true},
		{name: "record sentinel", err: errRecordNotFound, want: true},
		{name: "wrapped record sentinel", err: errors.Join(errors.New("load run"), errRecordNotFound), want: true},
		{name: "other", err: errors.New("load run"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNoRows(tt.err); got != tt.want {
				t.Fatalf("isNoRows() = %v, want %v", got, tt.want)
			}
		})
	}
}
