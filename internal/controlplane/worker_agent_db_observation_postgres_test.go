//go:build computerproof

package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbpool"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgxpool"
)

type nativeDBTest struct {
	pool     *pgxpool.Pool
	observer *nativeDBObserver
	output   *bytes.Buffer
	finish   func()
}

func newNativeDBTest(t *testing.T, maxConns int32) nativeDBTest {
	t.Helper()
	database := dbtest.Open(t)
	output := &bytes.Buffer{}
	observer, err := newNativeDBObserver(t.Context(), database.DSN, output)
	if err != nil {
		t.Fatal(err)
	}
	finish := sync.OnceFunc(func() {
		if err := observer.Close(); err != nil {
			t.Error(err)
		}
	})
	// Even construction failures must cancel and join the sampler.
	t.Cleanup(finish)
	config, err := pgxpool.ParseConfig(database.DSN)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns, config.MinConns = maxConns, 0
	config.ConnConfig.Tracer = observer
	pool, err := dbpool.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return nativeDBTest{pool, observer, output, finish}
}

func (f nativeDBTest) records(t *testing.T) []nativeDBObservation {
	t.Helper()
	f.finish()
	decoder := json.NewDecoder(bytes.NewReader(f.output.Bytes()))
	var records []nativeDBObservation
	for {
		var record nativeDBObservation
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func nativeDBRequest(ctx context.Context, save string) *http.Request {
	body, _ := json.Marshal(workerapi.AgentSavePublication{Save: workerapi.AgentSave{SaveID: save}})
	return httptest.NewRequest(http.MethodPost, "/worker/v1/computer-saves/capture", bytes.NewReader(body)).WithContext(ctx)
}

func TestNativeDBObserverCountsSQLAndPreservesSafeSaveIdentity(t *testing.T) {
	f := newNativeDBTest(t, 2)
	save, requestID := uuid.NewV7().String(), uuid.NewV7().String()
	body := `{"save":{"save_id":"` + save + `"},"inspection":{"private":"PRIVATE_BODY"}}`
	f.observer.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil || string(got) != body {
			t.Errorf("observer changed request body: %v", err)
		}
		w.Header().Set(requestIDHeader, requestID)
		var value string
		if err := f.pool.QueryRow(r.Context(), `SELECT $1::text`, "PRIVATE_ARGUMENT").Scan(&value); err != nil {
			t.Error(err)
		}
		if _, err := f.pool.Exec(r.Context(), `SELECT 1`); err != nil {
			t.Error(err)
		}
		if _, err := f.pool.Exec(r.Context(), `SELECT PRIVATE_SQL_IDENTIFIER`); err == nil {
			t.Error("expected observed query failure")
		}
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/worker/v1/computer-saves/objects/register?PRIVATE_URL=1", strings.NewReader(body)))
	// An observer/probe query has no request scope and cannot inflate a Save.
	if _, err := f.pool.Exec(t.Context(), `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	f.observer.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := f.pool.Exec(r.Context(), `SELECT 1`); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(workerapi.AgentSavePending{Save: &workerapi.AgentSave{SaveID: save}})
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/worker/v1/computer-saves/next", nil))
	records := f.records(t)
	if len(records) != 2 || records[0].SaveID != save || records[0].RequestID != requestID || records[0].SQLCalls != 3 || records[0].SQLFailures != 1 || records[0].SQLDurationMS <= 0 || records[0].PoolAcquisitions != 3 || records[0].PoolAcquireMS <= 0 || records[0].LockSamples != 0 || records[1].SaveID != save || records[1].Operation != "save_discover" || records[1].SQLCalls != 1 {
		t.Fatalf("SQL request evidence %+v", records)
	}
	if bytes.Contains(f.output.Bytes(), []byte("PRIVATE")) {
		t.Fatal("SQL, arguments, URL or request payload escaped into evidence")
	}
}

func TestNativeDBObserverCloseJoinsAdmittedRequestAndSampler(t *testing.T) {
	f := newNativeDBTest(t, 1)
	entered, joined := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		defer close(joined)
		f.observer.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
		})).ServeHTTP(httptest.NewRecorder(), nativeDBRequest(ctx, uuid.NewV7().String()))
	}()
	<-entered
	closed := make(chan struct{})
	go func() { f.finish(); close(closed) }()
	defer func() { cancel(); <-joined; <-closed }()
	select {
	case <-closed:
		t.Fatal("observer closed its evidence before its admitted request joined")
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	<-closed
	if records := f.records(t); len(records) != 1 {
		t.Fatalf("close discarded final request evidence %+v", records)
	}
}

func TestNativeDBObserverMeasuresCancelledPoolAcquisition(t *testing.T) {
	f := newNativeDBTest(t, 1)
	held, err := f.pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	ctx, cancel := context.WithCancel(t.Context())
	started, done := make(chan struct{}), make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		var queryErr error
		f.observer.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			_, queryErr = f.pool.Exec(r.Context(), `SELECT 1`)
		})).ServeHTTP(httptest.NewRecorder(), nativeDBRequest(ctx, uuid.NewV7().String()))
		done <- queryErr
	}()
	// Join the complete HTTP observation, not just its inner query callback.
	defer func() { cancel(); <-joined }()
	<-started
	timer := time.NewTimer(60 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	cancel()
	<-joined
	if err := <-done; err != context.Canceled {
		t.Fatalf("pool acquisition cancellation: %v", err)
	}
	records := f.records(t)
	if len(records) != 1 || records[0].PoolAcquisitions != 1 || records[0].PoolAcquireFailures != 1 || records[0].PoolAcquireMS < 40 || records[0].SQLCalls != 0 || records[0].LockSamples != 0 {
		t.Fatalf("pool queue was confused with SQL/row lock wait: %+v", records)
	}
}

func TestNativeDBObserverAttributesSampledRowLockToExactSave(t *testing.T) {
	f := newNativeDBTest(t, 3)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := f.pool.Exec(ctx, `CREATE TABLE native_db_lock(id int PRIMARY KEY,value int); INSERT INTO native_db_lock VALUES(1,0)`); err != nil {
		t.Fatal(err)
	}
	blocker, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `UPDATE native_db_lock SET value=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	blockedSave, otherSave := uuid.NewV7().String(), uuid.NewV7().String()
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		var queryErr error
		f.observer.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, queryErr = f.pool.Exec(r.Context(), `UPDATE native_db_lock SET value=2 WHERE id=1`)
		})).ServeHTTP(httptest.NewRecorder(), nativeDBRequest(ctx, blockedSave))
		done <- queryErr
	}()
	defer func() { cancel(); <-joined }()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		f.observer.mu.Lock()
		observed := false
		for _, query := range f.observer.active {
			observed = observed || query.scope.SaveID == blockedSave && query.scope.LockSamples >= 3
		}
		f.observer.mu.Unlock()
		if observed {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("row lock samples were not observed")
		case <-ticker.C:
		}
	}
	f.observer.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := f.pool.Exec(r.Context(), `SELECT 1`); err != nil {
			t.Error(err)
		}
	})).ServeHTTP(httptest.NewRecorder(), nativeDBRequest(ctx, otherSave))
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	<-joined
	if err != nil {
		t.Fatal(err)
	}
	records := f.records(t)
	if len(records) != 2 {
		t.Fatalf("row lock observation count %+v", records)
	}
	bySave := map[string]nativeDBObservation{}
	for _, record := range records {
		bySave[record.SaveID] = record
	}
	blocked, other := bySave[blockedSave], bySave[otherSave]
	if blocked.SQLCalls != 1 || blocked.LockSamples < 3 || blocked.LockMaxObservedSpanMS <= 0 || blocked.LockMaxObservationGapMS <= 0 || blocked.LockSampleIntervalMS != 20 || other.SQLCalls != 1 || other.LockSamples != 0 || other.LockMaxObservedSpanMS != 0 {
		t.Fatalf("row lock crossed Save identity: %+v", records)
	}
}

type nativeDBFailedWriter struct{}

func (nativeDBFailedWriter) Write([]byte) (int, error) { return 0, errors.New("PRIVATE_WRITE_FAILURE") }

func TestNativeDBObserverReportsTerminalFailureBeforeClose(t *testing.T) {
	for _, failure := range []string{"sampler", "writer"} {
		t.Run(failure, func(t *testing.T) {
			database := dbtest.Open(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var output io.Writer = &bytes.Buffer{}
			if failure == "writer" {
				output = nativeDBFailedWriter{}
			}
			observer, err := newNativeDBObserver(ctx, database.DSN, output)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				cancel()
				if err := observer.Close(); err == nil || strings.Contains(err.Error(), "PRIVATE") {
					t.Errorf("terminal observation failure not retained safely: %v", err)
				}
			}()
			if failure == "writer" {
				observer.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), nativeDBRequest(ctx, uuid.NewV7().String()))
			} else {
				config, err := pgxpool.ParseConfig(database.DSN)
				if err != nil {
					t.Fatal(err)
				}
				config.ConnConfig.Tracer = observer
				pool, err := dbpool.New(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer pool.Close()
				joined := make(chan struct{})
				go func() {
					defer close(joined)
					observer.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						pool.Exec(r.Context(), `SELECT pg_sleep(30)`)
					})).ServeHTTP(httptest.NewRecorder(), nativeDBRequest(ctx, uuid.NewV7().String()))
				}()
				defer func() { cancel(); <-joined }()
				for {
					observer.mu.Lock()
					active := len(observer.active) > 0
					observer.mu.Unlock()
					if active {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal("request query did not start")
					case <-time.After(time.Millisecond):
					}
				}
				// A real sampler query now fails while the request remains live.
				observer.sampler.Close()
				if err := observer.Failure(ctx); err == nil || ctx.Err() != nil {
					t.Fatalf("sampler failure waited for shutdown: %v", err)
				}
				select {
				case <-joined:
					t.Fatal("request ended before terminal sampler failure was reported")
				default:
				}
				return
			}
			if err := observer.Failure(ctx); err == nil || ctx.Err() != nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("writer failure waited for shutdown or leaked details: %v", err)
			}
		})
	}
}

func TestNativeDBObserverIdentifiesSessionAuthorityAndControl(t *testing.T) {
	f := newNativeDBTest(t, 1)
	cases := []struct {
		path, operation string
		status          int
	}{
		{"/worker/v1/sessions/authority", "session_authority", http.StatusOK},
		{"/worker/v1/sessions/attachment", "session_attachment", http.StatusConflict},
		{"/worker/v1/sessions/control", "session_control", http.StatusServiceUnavailable},
		{"/worker/v1/sessions/control/receipt", "session_control_receipt", http.StatusNoContent},
		{"/PRIVATE_URL", "other", http.StatusNotFound},
	}
	const body = `{"session_id":"PRIVATE_SESSION","token":"PRIVATE_TOKEN"}`
	for _, test := range cases {
		recorder := httptest.NewRecorder()
		f.observer.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, err := io.ReadAll(r.Body)
			if err != nil || string(got) != body {
				t.Errorf("observer changed authority request: %v", err)
			}
			w.WriteHeader(test.status)
		})).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, test.path+"?PRIVATE_QUERY", strings.NewReader(body)))
		if recorder.Code != test.status {
			t.Fatalf("observer changed response status: %d", recorder.Code)
		}
	}
	records := f.records(t)
	if len(records) != len(cases) {
		t.Fatalf("record count: %d", len(records))
	}
	for i, test := range cases {
		if records[i].Operation != test.operation || records[i].Status != test.status || records[i].SaveID != "" {
			t.Errorf("session operation evidence: %+v", records[i])
		}
	}
	if bytes.Contains(f.output.Bytes(), []byte("PRIVATE")) {
		t.Fatal("request content escaped into evidence")
	}
}
