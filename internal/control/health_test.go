package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestHealthzDoesNotRequireReadinessDB(t *testing.T) {
	handler := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestReadyzRequiresReadinessDB(t *testing.T) {
	handler := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestReadyzChecksSchemaVersion(t *testing.T) {
	currentVersion := int(mustSchemaCurrentVersion(t))
	tests := map[string]struct {
		row  pgx.Row
		want int
	}{
		"ready": {
			row:  fakeReadinessRow{version: currentVersion, ready: true},
			want: http.StatusOK,
		},
		"dirty": {
			row:  fakeReadinessRow{version: currentVersion, dirty: true},
			want: http.StatusServiceUnavailable,
		},
		"old version": {
			row:  fakeReadinessRow{version: currentVersion - 1},
			want: http.StatusServiceUnavailable,
		},
		"future version": {
			row:  fakeReadinessRow{version: currentVersion + 1, ready: true},
			want: http.StatusOK,
		},
		"query error": {
			row:  fakeReadinessRow{err: errors.New("relation does not exist")},
			want: http.StatusServiceUnavailable,
		},
		"component not ready": {
			row:  fakeReadinessRow{version: currentVersion, ready: false},
			want: http.StatusServiceUnavailable,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			handler := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DBTX: fakeReadinessDB{row: tt.row}})
			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Fatalf("status = %d want=%d body=%s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

func mustSchemaCurrentVersion(t *testing.T) uint {
	t.Helper()
	version, err := schema.CurrentVersion()
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func TestReadyzDoesNotExposeFailureDetails(t *testing.T) {
	handler := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DBTX: fakeReadinessDB{
		row: fakeReadinessRow{err: errors.New("relation schema_migrations does not exist")},
	}})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["status"] != "not_ready" {
		t.Fatalf("response = %+v", response)
	}
	if _, ok := response["error"]; ok {
		t.Fatalf("response exposed error details: %+v", response)
	}
}

type fakeReadinessDB struct {
	row pgx.Row
}

func (db fakeReadinessDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("unexpected Exec")
}

func (db fakeReadinessDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("unexpected Query")
}

func (db fakeReadinessDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return db.row
}

func (db fakeReadinessDB) Begin(context.Context) (pgx.Tx, error) {
	panic("unexpected Begin")
}

type fakeReadinessRow struct {
	version int
	dirty   bool
	ready   bool
	err     error
}

func (row fakeReadinessRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	if len(dest) == 5 {
		if workerGroupID, ok := dest[0].(*string); ok {
			*workerGroupID = "us-east-1-worker-group-1"
		} else {
			return errors.New("worker group id destination is not *string")
		}
		if state, ok := dest[1].(*db.WorkerGroupState); ok {
			*state = db.WorkerGroupStateActive
		} else {
			return errors.New("worker group state destination is not *db.WorkerGroupState")
		}
		if healthState, ok := dest[2].(*db.WorkerGroupHealthState); ok {
			*healthState = db.WorkerGroupHealthStateHealthy
		} else {
			return errors.New("health state destination is not *db.WorkerGroupHealthState")
		}
		if freshUntil, ok := dest[3].(*pgtype.Timestamptz); ok {
			*freshUntil = pgtype.Timestamptz{Valid: true}
		} else {
			return errors.New("fresh until destination is not *pgtype.Timestamptz")
		}
		if ready, ok := dest[4].(*pgtype.Bool); ok {
			*ready = pgtype.Bool{Bool: row.ready, Valid: true}
		} else {
			return errors.New("ready destination is not *pgtype.Bool")
		}
		return nil
	}
	if len(dest) != 2 {
		return errors.New("expected version and dirty destinations")
	}
	version, ok := dest[0].(*int)
	if !ok {
		return errors.New("version destination is not *int")
	}
	dirty, ok := dest[1].(*bool)
	if !ok {
		return errors.New("dirty destination is not *bool")
	}
	*version = row.version
	*dirty = row.dirty
	return nil
}
