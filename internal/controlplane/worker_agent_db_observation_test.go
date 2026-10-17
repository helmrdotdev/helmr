//go:build computerproof

package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
	"uuid"

	"github.com/felixge/httpsnoop"
	"github.com/helmrdotdev/helmr/internal/db/dbpool"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const nativeDBSampleEvery = 20 * time.Millisecond

// These are driver-call counts (including transaction commands), not protocol
// round trips or server-internal statements. SQL duration includes execution,
// network and lock acquisition. Pool acquisition can also include connection
// creation. Lock samples and their consecutive observed spans are deliberately
// separate: neither is an exact integrated PostgreSQL lock-wait duration.
type nativeDBObservation struct {
	StartedAt               time.Time `json:"started_at"`
	RequestDurationMS       float64   `json:"request_duration_ms"`
	RequestID               string    `json:"request_id"`
	SaveID                  string    `json:"save_id,omitempty"`
	Operation               string    `json:"operation"`
	Status                  int       `json:"status"`
	SQLCalls                int64     `json:"sql_calls"`
	SQLDurationMS           float64   `json:"sql_duration_ms"`
	SQLFailures             int64     `json:"sql_failures"`
	PoolAcquisitions        int64     `json:"pool_acquisitions"`
	PoolAcquireMS           float64   `json:"pool_acquire_ms"`
	PoolAcquireFailures     int64     `json:"pool_acquire_failures"`
	LockSampleIntervalMS    float64   `json:"lock_sample_interval_ms"`
	LockSamples             int64     `json:"lock_samples"`
	LockMaxObservedSpanMS   float64   `json:"lock_max_observed_span_ms"`
	LockMaxObservationGapMS float64   `json:"lock_max_observation_gap_ms"`
}

type nativeDBScopeKey struct{}
type nativeDBQueryKey struct{}
type nativeDBAcquireKey struct{}
type nativeDBQuery struct {
	scope              *nativeDBObservation
	started, firstLock time.Time
}

// One sampler and one private connection belong to each native CP incarnation.
// Its SQL has no tracer and cannot contaminate the request counts. Only active
// query PIDs enter its fixed query, never SQL text, arguments or credentials.
type nativeDBObserver struct {
	mu       sync.Mutex
	active   map[uint32]*nativeDBQuery
	encoder  *json.Encoder
	writeErr error
	cancel   context.CancelFunc
	joined   chan error
	closing  bool
	requests sync.WaitGroup
	sampler  *pgxpool.Pool
	failure  chan error
	failed   sync.Once
}

func newNativeDBObserver(ctx context.Context, dsn string, output io.Writer) (*nativeDBObserver, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("parse native database observation configuration")
	}
	config.MaxConns, config.MinConns = 1, 0
	pool, err := dbpool.New(ctx, config)
	if err != nil {
		return nil, errors.New("open native database observation pool")
	}
	ctx, cancel := context.WithCancel(ctx)
	o := &nativeDBObserver{active: map[uint32]*nativeDBQuery{}, encoder: json.NewEncoder(output), cancel: cancel, joined: make(chan error, 1), sampler: pool, failure: make(chan error, 1)}
	go func() {
		err := o.sample(ctx, pool)
		if err != nil {
			o.fail(err)
		}
		pool.Close()
		o.joined <- err
	}()
	return o, nil
}

// Health notification never consumes the separate sampler join result. The
// native proof must fail immediately even when its process is later SIGKILLed.
func (o *nativeDBObserver) fail(err error) { o.failed.Do(func() { o.failure <- err }) }

func (o *nativeDBObserver) Failure(ctx context.Context) error {
	select {
	case err := <-o.failure:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// The caller first cancels HTTP work. Close joins admitted handlers and the
// sampler before its caller can close the evidence file.
func (o *nativeDBObserver) Close() error {
	o.mu.Lock()
	o.closing = true
	o.mu.Unlock()
	o.cancel()
	o.requests.Wait()
	err := <-o.joined
	o.mu.Lock()
	defer o.mu.Unlock()
	return errors.Join(err, o.writeErr)
}

func (o *nativeDBObserver) TraceQueryStart(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	scope, _ := ctx.Value(nativeDBScopeKey{}).(*nativeDBObservation)
	if scope == nil {
		return ctx
	}
	query := &nativeDBQuery{scope: scope, started: time.Now()}
	o.mu.Lock()
	scope.SQLCalls++
	o.active[conn.PgConn().PID()] = query
	o.mu.Unlock()
	return context.WithValue(ctx, nativeDBQueryKey{}, query)
}

func (o *nativeDBObserver) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	query, _ := ctx.Value(nativeDBQueryKey{}).(*nativeDBQuery)
	if query == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	query.scope.SQLDurationMS += nativeDBMilliseconds(time.Since(query.started))
	if data.Err != nil {
		query.scope.SQLFailures++
	}
	if o.active[conn.PgConn().PID()] == query {
		delete(o.active, conn.PgConn().PID())
	}
}

func (o *nativeDBObserver) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return context.WithValue(ctx, nativeDBAcquireKey{}, time.Now())
}

func (o *nativeDBObserver) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	scope, _ := ctx.Value(nativeDBScopeKey{}).(*nativeDBObservation)
	started, _ := ctx.Value(nativeDBAcquireKey{}).(time.Time)
	if scope == nil || started.IsZero() {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	scope.PoolAcquisitions++
	scope.PoolAcquireMS += nativeDBMilliseconds(time.Since(started))
	if data.Err != nil {
		scope.PoolAcquireFailures++
	}
}

func (o *nativeDBObserver) sample(ctx context.Context, pool *pgxpool.Pool) error {
	ticker := time.NewTicker(nativeDBSampleEvery)
	defer ticker.Stop()
	previous := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		o.mu.Lock()
		active := make(map[uint32]*nativeDBQuery, len(o.active))
		pids := make([]int64, 0, len(o.active))
		for pid, query := range o.active {
			active[pid] = query
			pids = append(pids, int64(pid))
		}
		o.mu.Unlock()
		locked := map[uint32]bool{}
		if len(pids) > 0 {
			queryCtx, cancel := context.WithTimeout(ctx, time.Second)
			rows, err := pool.Query(queryCtx, `SELECT pid FROM pg_stat_activity WHERE pid=ANY($1::bigint[]) AND wait_event_type='Lock'`, pids)
			if err == nil {
				var values []uint32
				values, err = pgx.CollectRows(rows, pgx.RowTo[uint32])
				for _, pid := range values {
					locked[pid] = true
				}
			}
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("native database lock sampler failed")
			}
		}
		at := time.Now()
		o.mu.Lock()
		for pid, query := range active {
			// A completed query or reused connection cannot donate its samples
			// to another request, even if the observer query replied late.
			if o.active[pid] != query {
				continue
			}
			query.scope.LockMaxObservationGapMS = max(query.scope.LockMaxObservationGapMS, nativeDBMilliseconds(at.Sub(previous)))
			if !locked[pid] {
				query.firstLock = time.Time{}
				continue
			}
			query.scope.LockSamples++
			if query.firstLock.IsZero() {
				query.firstLock = at
			}
			query.scope.LockMaxObservedSpanMS = max(query.scope.LockMaxObservedSpanMS, nativeDBMilliseconds(at.Sub(query.firstLock)))
		}
		o.mu.Unlock()
		previous = at
	}
}

func nativeDBMilliseconds(value time.Duration) float64 {
	return float64(value) / float64(time.Millisecond)
}

func (o *nativeDBObserver) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		if o.closing {
			o.mu.Unlock()
			next.ServeHTTP(w, r)
			return
		}
		o.requests.Add(1)
		o.mu.Unlock()
		defer o.requests.Done()
		started := time.Now()
		scope := &nativeDBObservation{StartedAt: started.UTC(), RequestID: uuid.NewV7().String(), Operation: "other", LockSampleIntervalMS: nativeDBMilliseconds(nativeDBSampleEvery)}
		switch r.URL.Path {
		case "/worker/v1/sessions/authority":
			scope.Operation = "session_authority"
		case "/worker/v1/sessions/attachment":
			scope.Operation = "session_attachment"
		case "/worker/v1/sessions/control":
			scope.Operation = "session_control"
		case "/worker/v1/sessions/control/receipt":
			scope.Operation = "session_control_receipt"
		case "/worker/v1/computer-saves/objects/register":
			scope.Operation = "save_register"
		case "/worker/v1/computer-saves/objects/certify":
			scope.Operation = "save_certify"
		case "/worker/v1/computer-saves/capture":
			scope.Operation = "save_capture"
		case "/worker/v1/computer-saves/publish":
			scope.Operation = "save_publish"
		case "/worker/v1/computer-saves/next":
			scope.Operation = "save_discover"
		}
		if (scope.Operation == "save_register" || scope.Operation == "save_certify" || scope.Operation == "save_capture" || scope.Operation == "save_publish") && r.Body != nil {
			scope.SaveID = nativeDBSaveIdentity(r)
		}
		var response bytes.Buffer
		responseTruncated := false
		if scope.Operation == "save_discover" {
			w = httpsnoop.Wrap(w, httpsnoop.Hooks{Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
				return func(p []byte) (int, error) {
					if !responseTruncated && response.Len()+len(p) <= 8192 {
						response.Write(p)
					} else {
						responseTruncated = true
					}
					return next(p)
				}
			}})
		}
		defer func() {
			scope.RequestDurationMS = nativeDBMilliseconds(time.Since(started))
			if id := nativeDBSafeID(w.Header().Get(requestIDHeader)); id != "" {
				scope.RequestID = id
			}
			if scope.Operation == "save_discover" && !responseTruncated {
				var pending workerapi.AgentSavePending
				if json.Unmarshal(response.Bytes(), &pending) == nil && pending.Save != nil {
					scope.SaveID = nativeDBSafeID(pending.Save.SaveID)
				}
			}
			o.mu.Lock()
			defer o.mu.Unlock()
			if err := o.encoder.Encode(scope); err != nil {
				o.writeErr = errors.New("retain native database observation")
				o.fail(o.writeErr)
			}
		}()
		requestCtx := context.WithValue(r.Context(), nativeDBScopeKey{}, scope)
		metrics := httpsnoop.CaptureMetrics(next, w, r.WithContext(requestCtx))
		scope.Status = metrics.Code
	})
}

func nativeDBSafeID(value string) string {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil() {
		return ""
	}
	return id.String()
}

// The typed worker request starts with its small Save identity. Restore every
// consumed byte, including decoder lookahead, without copying object payloads.
func nativeDBSaveIdentity(r *http.Request) string {
	original := r.Body
	var prefix bytes.Buffer
	decoder := json.NewDecoder(io.TeeReader(io.LimitReader(original, 65536), &prefix))
	defer func() {
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(prefix.Bytes()), original), original}
	}()
	if token, err := decoder.Token(); err == nil && token == json.Delim('{') && decoder.More() {
		if key, err := decoder.Token(); err == nil && key == "save" {
			var save workerapi.AgentSave
			if decoder.Decode(&save) == nil {
				return nativeDBSafeID(save.SaveID)
			}
		}
	}
	return ""
}
