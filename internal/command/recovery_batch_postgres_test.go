package command

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// revokedCommands seeds running Commands on a Computer whose Secret binding is
// revoked, so every one is a recovery candidate.
func revokedCommands(t *testing.T, f agenttest.Fixture, count int) []uuid.UUID {
	t.Helper()
	secretID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO secrets(id,environment_id,name,status,revoked_at,revocation_generation) VALUES($1,$2,'TOKEN','revoked',clock_timestamp(),1)`, secretID, f.Environment)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_secret_bindings(environment_id,computer_id,secret_id,placement_kind,placement_target,mode) VALUES($1,$2,$3,'env','TOKEN','raw')`, f.Environment, f.Computer, secretID)
	ids := make([]uuid.UUID, 0, count)
	for i := range count {
		id := uuid.NewV7()
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(environment_id,id,computer_id,computer_lease_epoch,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id,status,updated_at)
 VALUES($1,$2,$3,1,ARRAY['true'],'{}','',60000,'user','fixture','running',clock_timestamp()+($4::int*interval '1 microsecond'))`, f.Environment, id, f.Computer, i)
		ids = append(ids, id)
	}
	return ids
}

func convergedCount(t *testing.T, f agenttest.Fixture) int {
	t.Helper()
	var n int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_commands WHERE environment_id=$1 AND cancel_requested_at IS NOT NULL`, f.Environment).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// poisonDB fails every statement that names one of the poisoned Commands, so
// those candidates fail persistently while the rest of the database works.
type poisonDB struct {
	*poisonExecutor
	pool db.TxDB
}

type poisonExecutor struct {
	inner    db.DBTX
	poisoned map[uuid.UUID]bool
	failures int
}

var errPoisoned = errors.New("injected recovery failure")

func (p *poisonExecutor) hit(args []any) bool {
	for _, arg := range args {
		switch v := arg.(type) {
		case uuid.UUID:
			if p.poisoned[v] {
				return true
			}
		case pgtype.UUID:
			if v.Valid && p.poisoned[uuid.UUID(v.Bytes)] {
				return true
			}
		}
	}
	return false
}

func (p *poisonExecutor) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if p.hit(args) {
		p.failures++
		return pgconn.CommandTag{}, errPoisoned
	}
	return p.inner.Exec(ctx, sql, args...)
}

func (p *poisonExecutor) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if p.hit(args) {
		p.failures++
		return nil, errPoisoned
	}
	return p.inner.Query(ctx, sql, args...)
}

func (p *poisonExecutor) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if p.hit(args) {
		p.failures++
		return poisonedRow{}
	}
	return p.inner.QueryRow(ctx, sql, args...)
}

type poisonedRow struct{}

func (poisonedRow) Scan(...any) error { return errPoisoned }

type poisonTx struct {
	pgx.Tx
	executor *poisonExecutor
}

func (p poisonTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return p.executor.Exec(ctx, sql, args...)
}

func (p poisonTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return p.executor.Query(ctx, sql, args...)
}

func (p poisonTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return p.executor.QueryRow(ctx, sql, args...)
}

func (p poisonDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return poisonTx{Tx: tx, executor: &poisonExecutor{inner: tx, poisoned: p.poisoned}}, nil
}

func newPoisonDB(pool db.TxDB, poisoned ...uuid.UUID) poisonDB {
	set := make(map[uuid.UUID]bool, len(poisoned))
	for _, id := range poisoned {
		set[id] = true
	}
	return poisonDB{poisonExecutor: &poisonExecutor{inner: pool, poisoned: set}, pool: pool}
}

// TestRecoveryPassVisitsEveryCandidateDespitePersistentFailures is the progress
// evidence behind retiring the targeted revocation notification: with more
// candidates than one page holds and failing candidates at the head of the
// order, one pass still reaches every other Command.
func TestRecoveryPassVisitsEveryCandidateDespitePersistentFailures(t *testing.T) {
	f := agenttest.New(t)
	const total, page = 600, 256
	ids := revokedCommands(t, f, total)
	poisoned := []uuid.UUID{ids[0], ids[1], ids[300]}
	database := newPoisonDB(f.Pool, poisoned...)

	pages := 0
	var position RecoveryPosition
	for {
		next, more, err := RecoverBatch(t.Context(), database, position, page)
		pages++
		if err != nil && !errors.Is(err, errPoisoned) {
			t.Fatalf("page %d: %v", pages, err)
		}
		if pages > total/page+2 {
			t.Fatalf("pass did not finish within %d pages", pages)
		}
		position = next
		if !more {
			break
		}
	}
	t.Logf("one pass over %d candidates took %d pages of %d", total, pages, page)
	if got := convergedCount(t, f); got != total-len(poisoned) {
		t.Fatalf("converged %d, want every candidate except the %d failing ones", got, len(poisoned))
	}
	for _, id := range poisoned {
		var requested bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT cancel_requested_at IS NOT NULL FROM computer_commands WHERE id=$1`, id).Scan(&requested); err != nil || requested {
			t.Fatalf("poisoned Command %s converged: %v %v", id, requested, err)
		}
	}
	// A second pass finds only the failing candidates again: bounded work,
	// still failing, still not blocking anything.
	next, more, err := RecoverBatch(t.Context(), database, RecoveryPosition{}, page)
	if !errors.Is(err, errPoisoned) || more || next != (RecoveryPosition{}) {
		t.Fatalf("second pass: %+v more=%v %v", next, more, err)
	}
	if got := convergedCount(t, f); got != total-len(poisoned) {
		t.Fatalf("second pass changed convergence: %d", got)
	}
}

// TestRecoveryPassRestartsFromTheBeginningAfterInterruption: a pass that stops
// mid-way (process restart) is restart-safe; the next pass from the zero
// position converges the remainder without touching recovered Commands twice.
func TestRecoveryPassRestartsFromTheBeginningAfterInterruption(t *testing.T) {
	f := agenttest.New(t)
	const total, page = 300, 100
	revokedCommands(t, f, total)
	position, more, err := RecoverBatch(t.Context(), f.Pool, RecoveryPosition{}, page)
	if err != nil || !more || position == (RecoveryPosition{}) {
		t.Fatalf("first page: %+v more=%v %v", position, more, err)
	}
	if got := convergedCount(t, f); got != page {
		t.Fatalf("first page converged %d", got)
	}
	interrupted, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := RecoverBatch(interrupted, f.Pool, position, page); err == nil {
		t.Fatal("interrupted page reported success")
	}
	pages := 0
	for position = (RecoveryPosition{}); ; pages++ {
		next, more, err := RecoverBatch(t.Context(), f.Pool, position, page)
		if err != nil {
			t.Fatalf("restarted pass: %v", err)
		}
		position = next
		if !more {
			break
		}
		if pages > total/page+1 {
			t.Fatalf("restarted pass did not finish")
		}
	}
	if got := convergedCount(t, f); got != total {
		t.Fatalf("converged %d after restart, want %d", got, total)
	}
	var live bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='active' AND fenced_at IS NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer).Scan(&live); err != nil || !live {
		t.Fatalf("recovery changed the physical lease: %v %v", live, err)
	}
}
