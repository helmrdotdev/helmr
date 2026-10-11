package telemetry

import (
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func diagnosticDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return agenttest.New(t).Pool
}
func diagnosticTestSource(t *testing.T, pool *pgxpool.Pool, kind string, env uuid.UUID) DiagnosticSource {
	t.Helper()
	var baseEnv uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT environment_id FROM session_processes LIMIT 1`).Scan(&baseEnv); err != nil {
		t.Fatal(err)
	}
	if env == uuid.Nil() {
		env = baseEnv
	}
	s := DiagnosticSource{EnvironmentID: env, Kind: kind, ID: uuid.NewV7(), ProducerEpoch: 1}
	if env != baseEnv {
		dbtest.MustExec(t, t.Context(), pool, `INSERT INTO environments(history_retention_mode,id,org_id,project_id,slug,name,color_hex) SELECT 'until_environment_deletion',$1::uuid,org_id,project_id,$1::text,'diagnostic test','#112233' FROM environments WHERE id=$2 ON CONFLICT(id) DO NOTHING`, env, baseEnv)
	}
	switch kind {
	case "session":
		if env != baseEnv {
			t.Fatal("Session test source requires fixture Environment")
		}
		dbtest.MustExec(t, t.Context(), pool, `INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth) SELECT 'until_environment_deletion',environment_id,$1,agent_id,deployment_id,computer_id,$1,0 FROM sessions WHERE environment_id=$2 LIMIT 1`, s.ID, env)
		dbtest.MustExec(t, t.Context(), pool, `INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status) SELECT environment_id,$1,1,computer_id,computer_lease_epoch,'ready' FROM session_processes WHERE environment_id=$2 LIMIT 1`, s.ID, env)
	case "computer_preparation":
		dbtest.MustExec(t, t.Context(), pool, `INSERT INTO computer_preparation_specs(environment_id,id,spec_digest,spec,seed) VALUES($1,$2::uuid,'sha256:'||repeat(replace($2::text,'-',''),2),'{}','{}')`, env, s.ID)
		dbtest.MustExec(t, t.Context(), pool, `INSERT INTO computer_preparations(environment_id,id,preparation_spec_id,retry_key,status,deadline_at,executor_epoch,worker_host_id,worker_epoch,instance_id,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest)
   SELECT $1::uuid,$2::uuid,$2::uuid,$2::text,'running',clock_timestamp()+interval '1 hour',1,worker_host_id,worker_epoch,$2,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest FROM computer_leases LIMIT 1`, env, s.ID)
	case "computer_command":
		dbtest.MustExec(t, t.Context(), pool, `WITH spec AS (INSERT INTO computer_preparation_specs(environment_id,id,spec_digest,spec,seed) VALUES($1,$2::uuid,'sha256:'||repeat(replace($2::text,'-',''),2),'{}','{}')) INSERT INTO computers(environment_id,id,preparation_spec_id,preparation_deadline_at) VALUES($1,$2,$2,clock_timestamp()+interval '1 hour')`, env, s.ID)
		dbtest.MustExec(t, t.Context(), pool, `INSERT INTO computer_commands(environment_id,id,computer_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id) VALUES($1,$2,$2,ARRAY['true'],'{}','',1000,'user','test')`, env, s.ID)
	default:
		t.Fatal("unsupported diagnostic test owner")
	}
	return s
}
func appendDiagnosticTx(t *testing.T, tx pgx.Tx, source DiagnosticSource, record diagnostic.Record, bounds diagnostic.Bounds) (DiagnosticReceipt, error) {
	t.Helper()
	admission, err := BeginDiagnosticAdmission(t.Context(), tx, source, bounds)
	if err != nil {
		return DiagnosticReceipt{}, err
	}
	return admission.Append(t.Context(), record)
}
func diagnosticTestBounds() diagnostic.Bounds {
	return diagnostic.Bounds{ChunkBytes: 3, SourceBytes: 9, SourceRecords: 3, EnvironmentBytes: 18, EnvironmentRecords: 6, QueueBytes: 36, QueueRecords: 12}
}
func appendDiagnosticTest(t *testing.T, pool *pgxpool.Pool, s DiagnosticSource, r diagnostic.Record, b diagnostic.Bounds) (DiagnosticReceipt, error) {
	t.Helper()
	var receipt DiagnosticReceipt
	err := db.RunTx(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		receipt, err = appendDiagnosticTx(t, tx, s, r, b)
		return err
	})
	return receipt, err
}
func TestDiagnosticAcceptanceImmutableRetryCoverageAndClosure(t *testing.T) {
	pool := diagnosticDatabase(t)
	source := diagnosticTestSource(t, pool, "session", uuid.Nil())
	record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: time.Now().UnixNano(), Data: []byte{0, 255, 128}}
	bounds := diagnosticTestBounds()
	first, err := appendDiagnosticTest(t, pool, source, record, bounds)
	if err != nil {
		t.Fatal(err)
	}
	// A new transaction after a discarded receipt must recover its immutable time.
	retry, err := appendDiagnosticTest(t, pool, source, record, bounds)
	if err != nil || !retry.AcceptedAt.Equal(first.AcceptedAt) || !retry.ExpiresAt.Equal(first.ExpiresAt) || retry.ThroughSequence != 1 || first.ExpiresAt.Sub(first.AcceptedAt) != 90*24*time.Hour {
		t.Fatalf("retry %+v %+v %v", first, retry, err)
	}
	changed := record
	changed.Data = []byte{0, 255, 127}
	if _, err = appendDiagnosticTest(t, pool, source, changed, bounds); !errors.Is(err, ErrDiagnosticConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	gap := diagnostic.Record{Stream: "stdout", Kind: "gap", Sequence: 2, ThroughSequence: 5, ObservedAtUnixNano: record.ObservedAtUnixNano + 1, DroppedBytes: 12}
	if _, err = appendDiagnosticTest(t, pool, source, gap, bounds); err != nil {
		t.Fatal(err)
	}
	end := diagnostic.Record{Stream: "stdout", Kind: "end", Sequence: 6, ThroughSequence: 6, ObservedAtUnixNano: record.ObservedAtUnixNano + 2, Complete: true}
	if _, err = appendDiagnosticTest(t, pool, source, end, bounds); err != nil {
		t.Fatal(err)
	}
	if _, err = appendDiagnosticTest(t, pool, source, end, bounds); err != nil {
		t.Fatalf("EOF retry: %v", err)
	}
	record.Sequence, record.ThroughSequence = 7, 7
	if _, err = appendDiagnosticTest(t, pool, source, record, bounds); !errors.Is(err, ErrDiagnosticClosed) {
		t.Fatalf("closed stream: %v", err)
	}
	var count, bytes int64
	if err = pool.QueryRow(t.Context(), `SELECT count(*),sum(octet_length(data)) FROM telemetry_outbox`).Scan(&count, &bytes); err != nil || count != 3 || bytes != 3 {
		t.Fatalf("coverage rows=%d bytes=%d err=%v", count, bytes, err)
	}
}

func TestDiagnosticAcceptanceCapacitySerializesConcurrentSources(t *testing.T) {
	pool := diagnosticDatabase(t)
	bounds := diagnostic.Bounds{ChunkBytes: 3, SourceBytes: 3, SourceRecords: 1, EnvironmentBytes: 6, EnvironmentRecords: 2, QueueBytes: 9, QueueRecords: 3}
	envs := []uuid.UUID{uuid.New(), uuid.New()}
	var jobs sync.WaitGroup
	results := make(chan error, 20)
	for i := range 20 {
		source := diagnosticTestSource(t, pool, "computer_command", envs[i%2])
		jobs.Go(func() {
			record := diagnostic.Record{Stream: "stderr", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte("abc")}
			_, err := appendDiagnosticTest(t, pool, source, record, bounds)
			results <- err
		})
	}
	jobs.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrDiagnosticCapacity) && !IsDiagnosticBusy(err) {
			t.Fatal(err)
		}
	}
	// Contention is retryable, so the concurrent wave may leave capacity unused.
	// Fill only that remaining capacity after contenders finish.
	if accepted > 3 {
		t.Fatalf("aggregate over-admitted %d", accepted)
	}
	for _, env := range envs {
		for range 3 {
			source := diagnosticTestSource(t, pool, "computer_command", env)
			record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte("abc")}
			first, err := appendDiagnosticTest(t, pool, source, record, bounds)
			if err == nil {
				accepted++
				retry, err := appendDiagnosticTest(t, pool, source, record, bounds)
				if err != nil || !retry.AcceptedAt.Equal(first.AcceptedAt) {
					t.Fatalf("retry at capacity: %+v %v", retry, err)
				}
			} else if !errors.Is(err, ErrDiagnosticCapacity) && !IsDiagnosticBusy(err) {
				t.Fatal(err)
			}
		}
	}
	if accepted != 3 {
		t.Fatalf("aggregate admitted %d", accepted)
	}
	var records, bytes, maxEnvironment int64
	if err := pool.QueryRow(t.Context(), `SELECT count(*),COALESCE(sum(ingest_size_bytes),0)::bigint FROM telemetry_outbox WHERE stream_kind='diagnostic'`).Scan(&records, &bytes); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT max(records) FROM (SELECT count(*) records FROM telemetry_outbox WHERE stream_kind='diagnostic' GROUP BY environment_id) occupied`).Scan(&maxEnvironment); err != nil {
		t.Fatal(err)
	}
	if records != 3 || bytes != 9 || maxEnvironment > 2 {
		t.Fatalf("capacity records=%d bytes=%d perEnvironment=%d", records, bytes, maxEnvironment)
	}
}

func TestDiagnosticAcceptanceRollbackAndExpiredRetry(t *testing.T) {
	pool := diagnosticDatabase(t)
	source := diagnosticTestSource(t, pool, "session", uuid.Nil())
	record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte("abc")}
	bounds := diagnosticTestBounds()
	rejected := errors.New("producer authority expired while waiting")
	err := db.RunTx(t.Context(), pool, func(tx pgx.Tx) error {
		if _, err := appendDiagnosticTx(t, tx, source, record, bounds); err != nil {
			return err
		}
		return rejected
	})
	if !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	var records int64
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM telemetry_outbox WHERE stream_kind='diagnostic'`).Scan(&records); err != nil || records != 0 {
		t.Fatalf("rollback count=%d err=%v", records, err)
	}
	if _, err = appendDiagnosticTest(t, pool, source, record, bounds); err != nil {
		t.Fatal(err)
	}
	// Ordinary payload absence must not destroy the latest retry witness.
	dbtest.MustExec(t, t.Context(), pool, `DELETE FROM telemetry_outbox`)
	retry, err := appendDiagnosticTest(t, pool, source, record, bounds)
	if err != nil || retry.Expired {
		t.Fatalf("retained latest witness %+v %v", retry, err)
	}
	changed := record
	changed.Data = []byte("abd")
	if _, err = appendDiagnosticTest(t, pool, source, changed, bounds); !errors.Is(err, ErrDiagnosticConflict) {
		t.Fatalf("changed pruned retry %v", err)
	}
	dbtest.MustExec(t, t.Context(), pool, `UPDATE session_processes SET stdout_last_accepted_at=stdout_last_accepted_at-interval '2200 hours',stdout_last_expires_at=stdout_last_expires_at-interval '2200 hours' WHERE session_id=$1`, source.ID)
	expired, err := appendDiagnosticTest(t, pool, source, record, bounds)
	if err != nil || !expired.Expired || expired.ThroughSequence != 1 || !expired.AcceptedAt.IsZero() || !expired.ExpiresAt.IsZero() {
		t.Fatalf("expired disposition %+v %v", expired, err)
	}
	var cleared bool
	if err = pool.QueryRow(t.Context(), `SELECT stdout_last_digest IS NULL AND stdout_expired_through=stdout_accepted_through FROM session_processes WHERE session_id=$1`, source.ID).Scan(&cleared); err != nil || !cleared {
		t.Fatalf("expired digest retained %v %v", cleared, err)
	}
	// A later wall-clock correction cannot revive already expired coverage.
	dbtest.MustExec(t, t.Context(), pool, `UPDATE session_processes SET stdout_last_accepted_at=statement_timestamp(),stdout_last_expires_at=statement_timestamp()+interval '2160 hours' WHERE session_id=$1`, source.ID)
	expired, err = appendDiagnosticTest(t, pool, source, record, bounds)
	if err != nil || !expired.Expired {
		t.Fatalf("expired prefix revived %+v %v", expired, err)
	}
}

func TestDiagnosticAcceptanceFixedPipesAndMonotonicClock(t *testing.T) {
	pool := diagnosticDatabase(t)
	source := diagnosticTestSource(t, pool, "session", uuid.Nil())
	bounds := diagnosticTestBounds()
	record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte("abc")}
	if _, err := appendDiagnosticTest(t, pool, source, record, bounds); err != nil {
		t.Fatal(err)
	}
	another := record
	another.Stream = "stderr"
	if _, err := appendDiagnosticTest(t, pool, source, another, bounds); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), pool, `UPDATE session_processes SET stdout_last_accepted_at=statement_timestamp()+interval '1 hour',stdout_last_expires_at=statement_timestamp()+interval '2161 hours' WHERE session_id=$1`, source.ID)
	var future time.Time
	if err := pool.QueryRow(t.Context(), `SELECT stdout_last_accepted_at FROM session_processes WHERE session_id=$1`, source.ID).Scan(&future); err != nil {
		t.Fatal(err)
	}
	record.Sequence, record.ThroughSequence = 2, 2
	second, err := appendDiagnosticTest(t, pool, source, record, bounds)
	if err != nil || !second.AcceptedAt.Equal(future) {
		t.Fatalf("clock regressed: %+v %v", second, err)
	}
	var offset int64
	if err := pool.QueryRow(t.Context(), `SELECT stdout_byte_offset FROM session_processes WHERE session_id=$1`, source.ID).Scan(&offset); err != nil || offset != 6 {
		t.Fatalf("offset=%d %v", offset, err)
	}
}
