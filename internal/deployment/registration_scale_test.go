package deployment

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"uuid"
)

func TestDeploymentMaximumDefinitionRegistrationAndPromotion(t *testing.T) {
	if os.Getenv("HELMR_TEST_DEPLOYMENT_SCALE") != "1" {
		t.Skip("HELMR_TEST_DEPLOYMENT_SCALE is not set")
	}
	t.Run("one-computer", func(t *testing.T) { measureDeploymentRegistration(t, 1, 0) })
	t.Run("maximum-computers-with-secrets", func(t *testing.T) { measureDeploymentRegistration(t, 256, 64) })
}

func measureDeploymentRegistration(t *testing.T, computerCount, secretCount int) {
	f := newDeploymentFinalizePostgresFixture(t)
	metadata := &f.request.bundle.bundle.Program.Metadata
	computer := metadata.Definitions[2]
	prototype := *metadata.Definitions[0].Agent
	prototype.Triggers = map[string]definition.CronTrigger{"minute": {Cron: "* * * * *", Timezone: "UTC", Input: []byte(`[{"type":"text","text":"{\"scheduled\":true}"}]`)}}
	refs := []definition.SecretBinding{}
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		for i := range secretCount {
			id, version := uuid.NewV7(), uuid.NewV7()
			name := fmt.Sprintf("TOKEN_%03d", i)
			if _, err := tx.Exec(t.Context(), `INSERT INTO secrets(id,environment_id,name,current_version_id) VALUES($1,$2,$3,$4); INSERT INTO secret_versions(id,secret_id,version,nonce,ciphertext) VALUES($4,$1,1,decode(repeat('00',12),'hex'),decode(repeat('00',16),'hex'))`, pgx.QueryExecModeSimpleProtocol, id, pgvalue.MustUUIDValue(f.request.environmentID), name, version); err != nil {
				return err
			}
			refs = append(refs, definition.SecretBinding{SecretID: id.String(), Env: &definition.SecretBindingEnv{Name: name, Mode: "raw"}})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	agentCount := definition.MaxBuildDefinitions - computerCount
	metadata.Definitions = make([]artifact.ProgramDefinition, definition.MaxBuildDefinitions)
	for i := range agentCount {
		agent := prototype
		agent.ComputerDefinitionID = fmt.Sprintf("computer-%03d", i%computerCount)
		metadata.Definitions[i] = artifact.ProgramDefinition{Kind: definition.KindAgent, DeclaredID: fmt.Sprintf("agent-%05d", i), Agent: &agent}
	}
	for i := range computerCount {
		declaration := computer
		manifest := *computer.Computer
		declaration.DeclaredID = fmt.Sprintf("computer-%03d", i)
		declaration.Computer = &manifest
		every := int64(1101)
		manifest.Refresh = &definition.ComputerRefresh{EveryMs: every}
		manifest.BuildSecrets = refs
		metadata.Definitions[agentCount+i] = declaration
	}
	counter := &statementCounter{base: f.pool}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	record, err := register(t.Context(), counter, f.request)
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("registration definitions=%d elapsed=%s statements=%d allocated_bytes=%d", len(metadata.Definitions), elapsed, counter.calls.Load(), after.TotalAlloc-before.TotalAlloc)
	if counter.calls.Load() > 20 {
		t.Fatalf("registration grew database round trips with Agent count: %d", counter.calls.Load())
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_definitions`).Scan(&count); err != nil || count != agentCount {
		t.Fatalf("registered=%d: %v", count, err)
	}
	counter.calls.Store(0)
	runtime.ReadMemStats(&before)
	started = time.Now()
	if _, err := Promote(t.Context(), counter, f.principal(), f.scope(), record.ID, nil); err != nil {
		t.Fatal(err)
	}
	elapsed = time.Since(started)
	runtime.ReadMemStats(&after)
	t.Logf("promotion schedules=%d elapsed=%s statements=%d allocated_bytes=%d", count, elapsed, counter.calls.Load(), after.TotalAlloc-before.TotalAlloc)
	if counter.calls.Load() > 15 {
		t.Fatalf("promotion grew database round trips with schedule count: %d", counter.calls.Load())
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_schedules WHERE active_until IS NULL`).Scan(&count); err != nil || count != agentCount {
		t.Fatalf("activated=%d: %v", count, err)
	}
}

type statementCounter struct {
	base  db.TxBeginner
	calls atomic.Int64
}

func (c *statementCounter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := c.base.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &countedTx{Tx: tx, calls: &c.calls}, nil
}

type countedTx struct {
	pgx.Tx
	calls *atomic.Int64
}

func (t *countedTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.calls.Add(1)
	return t.Tx.Exec(ctx, sql, args...)
}
func (t *countedTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	t.calls.Add(1)
	return t.Tx.Query(ctx, sql, args...)
}
func (t *countedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	t.calls.Add(1)
	return t.Tx.QueryRow(ctx, sql, args...)
}
