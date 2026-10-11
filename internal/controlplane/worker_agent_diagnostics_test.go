package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type workerDiagnosticBeginFailure struct{ cause error }

func (b workerDiagnosticBeginFailure) Begin(context.Context) (pgx.Tx, error) { return nil, b.cause }

func TestWorkerAgentTransactionDiagnosticsKeepHTTPGeneric(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		cause      error
	}{
		{"cancelled", "context_cancelled", fmt.Errorf("private connection: %w", context.Canceled)},
		{"deadline", "deadline_exceeded", fmt.Errorf("private connection: %w", context.DeadlineExceeded)},
		{"postgres", "postgres", &pgconn.PgError{Code: "53300", Message: "private connection", Detail: "private SQL"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := db.RunTx(t.Context(), workerDiagnosticBeginFailure{tc.cause}, func(pgx.Tx) error { t.Fatal("unexpected transaction body"); return nil })
			var logs bytes.Buffer
			server := &Server{log: slog.New(slog.NewJSONHandler(&logs, nil))}
			response := httptest.NewRecorder()
			server.writeAgentWorkerError(response, err)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d", response.Code)
			}
			body := response.Body.String()
			if !strings.Contains(body, `"code":"service_unavailable","message":"service is unavailable"`) || strings.Contains(body, "private") || strings.Contains(body, "53300") || strings.Contains(body, "begin transaction") {
				t.Fatalf("public response exposed diagnostics: %s", body)
			}
			var record struct {
				Error struct{ Stage, Cause, SQLState string }
			}
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			expectedState := ""
			if tc.name == "postgres" {
				expectedState = "53300"
			}
			if record.Error.Stage != "begin transaction" || record.Error.Cause != tc.kind || record.Error.SQLState != expectedState || strings.Contains(logs.String(), "private") {
				t.Fatalf("missing or unsafe diagnostic: %s", logs.String())
			}
		})
	}
}

type workerDiagnosticCommitFailure struct{ pgx.Tx }

func (workerDiagnosticCommitFailure) Begin(context.Context) (pgx.Tx, error) {
	return workerDiagnosticCommitFailure{}, nil
}
func (workerDiagnosticCommitFailure) Commit(context.Context) error {
	return fmt.Errorf("private connection: %w", context.Canceled)
}
func (workerDiagnosticCommitFailure) Rollback(context.Context) error { return pgx.ErrTxClosed }

func TestWorkerAgentJoinedTransactionDiagnosticsKeepHTTPGeneric(t *testing.T) {
	err := db.RunTx(t.Context(), workerDiagnosticCommitFailure{}, func(pgx.Tx) error { return nil })
	if !errors.Is(err, context.Canceled) || !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("lost failure identity: %v", err)
	}
	var logs bytes.Buffer
	server := &Server{log: slog.New(slog.NewJSONHandler(&logs, nil))}
	response := httptest.NewRecorder()
	server.writeAgentWorkerError(response, err)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"service_unavailable","message":"service is unavailable"`) {
		t.Fatalf("HTTP failure contract: %d %s", response.Code, response.Body.String())
	}
	for _, text := range []string{"private", "commit transaction", "rollback transaction", "context_cancelled"} {
		if strings.Contains(response.Body.String(), text) {
			t.Fatalf("HTTP exposed diagnostic %q", text)
		}
	}
	var record struct {
		Error struct {
			Operation, Rollback struct{ Stage, Cause string }
		}
	}
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.Error.Operation.Stage != "commit transaction" || record.Error.Operation.Cause != "context_cancelled" ||
		record.Error.Rollback.Stage != "rollback transaction" || record.Error.Rollback.Cause != "transaction_closed" || strings.Contains(logs.String(), "private") {
		t.Fatalf("missing or unsafe joined diagnostics: %s", logs.String())
	}
}
