package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/keyedhash"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresClaimProtocol(t *testing.T) {
	pool := openClaimPostgres(t)
	environmentID := seedClaimEnvironment(t, pool)
	queries := db.New(pool)
	managerV1 := validatedClaimManager(t, 1, pool, queries)
	request := secretCreateRequest(t, environmentID, "request-key", "value")

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	claims, err := managerV1.Transaction(tx)
	if err != nil {
		t.Fatal(err)
	}
	created, err := claims.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claims.Complete(t.Context(), created.Claim, []byte(`{"secretId":"sec_1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	var lifetimeSeconds int64
	if err := pool.QueryRow(t.Context(), `
		SELECT extract(epoch FROM expires_at - accepted_at)::bigint
		  FROM idempotency_claims
		 WHERE id = $1
	`, created.Claim.ID).Scan(&lifetimeSeconds); err != nil {
		t.Fatal(err)
	}
	if lifetimeSeconds != int64((30*24*time.Hour)/time.Second) {
		t.Fatalf("claim lifetime = %d seconds", lifetimeSeconds)
	}

	managerV2 := validatedClaimManager(t, 2, pool, queries)
	staleHashes, err := keyedhash.New(map[int32][]byte{
		1: bytes.Repeat([]byte{1}, keyedhash.KeySize),
	})
	if err != nil {
		t.Fatal(err)
	}
	staleManager := New(staleHashes)
	tx, err = pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	staleClaims, err := staleManager.Transaction(tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := staleClaims.Acquire(t.Context(), secretCreateRequest(t, environmentID, "stale-key", "value")); err == nil {
		t.Fatal("expected stale process to fail closed")
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	results := make(chan Result, 2)
	errs := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			tx, err := pool.Begin(context.Background())
			if err != nil {
				errs <- err
				return
			}
			defer tx.Rollback(context.Background())
			claims, err := managerV2.Transaction(tx)
			if err != nil {
				errs <- err
				return
			}
			result, err := claims.Acquire(context.Background(), request)
			if err != nil {
				errs <- err
				return
			}
			if err := tx.Commit(context.Background()); err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	close(results)
	for result := range results {
		if result.New || result.Claim.ID != created.Claim.ID || result.Claim.HashKeyVersion != 1 {
			t.Fatalf("concurrent replay = %+v", result)
		}
	}

	if _, err := pool.Exec(t.Context(), `
		UPDATE idempotency_claims
		   SET accepted_at = statement_timestamp() - interval '31 days',
		       expires_at = statement_timestamp() - interval '1 day'
		 WHERE id = $1
	`, created.Claim.ID); err != nil {
		t.Fatal(err)
	}
	reboundRequest := secretCreateRequest(t, environmentID, "request-key", "next-value")
	tx, err = pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	claims, err = managerV2.Transaction(tx)
	if err != nil {
		t.Fatal(err)
	}
	rebound, err := claims.Acquire(t.Context(), reboundRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !rebound.New || rebound.Claim.Generation != 2 || rebound.Claim.HashKeyVersion != 2 {
		t.Fatalf("rebound = %+v", rebound)
	}

	tx, err = pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	claims, err = managerV2.Transaction(tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claims.Complete(t.Context(), created.Claim, []byte(`{"secretId":"late"}`)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("late completion error = %v", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	rollbackRequest := secretCreateRequest(t, environmentID, "rollback-key", "value")
	tx, err = pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	claims, err = managerV2.Transaction(tx)
	if err != nil {
		t.Fatal(err)
	}
	rolledBack, err := claims.Acquire(t.Context(), rollbackRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	claims, err = managerV2.Transaction(tx)
	if err != nil {
		t.Fatal(err)
	}
	afterRollback, err := claims.Acquire(t.Context(), rollbackRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !afterRollback.New || afterRollback.Claim.Generation != 1 || afterRollback.Claim.ID == rolledBack.Claim.ID {
		t.Fatalf("after rollback = %+v rolled back = %+v", afterRollback, rolledBack)
	}

	hashes := claimTestKeys(t)
	authority := keyedhash.NewAuthority(hashes)
	tx, err = pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Retire(t.Context(), tx, 1); err == nil {
		t.Fatal("expected referenced lookup HMAC version retirement to fail")
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	collected, err := queries.CollectRetiredIdempotencyClaims(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(collected) == 0 {
		t.Fatal("expected retired idempotency claim collection")
	}
	tx, err = pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	retired, err := authority.Retire(t.Context(), tx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !retired.RetiredAt.Valid {
		t.Fatalf("retired version = %+v", retired)
	}
	tx, err = pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Activate(t.Context(), tx, 1); err == nil {
		t.Fatal("expected retired lookup HMAC version reactivation to fail")
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresActorInputClaimCanonicalReplayAndConflict(t *testing.T) {
	pool := openClaimPostgres(t)
	environmentID := seedClaimEnvironment(t, pool)
	manager := validatedClaimManager(t, 1, pool, db.New(pool))
	actorID := uuid.New()

	acquire := func(input string) (Result, error) {
		t.Helper()
		request, err := NewActorInputSendRequest(
			environmentID,
			actorID,
			"message:1",
			[]byte(input),
		)
		if err != nil {
			return Result{}, err
		}
		tx, err := pool.Begin(t.Context())
		if err != nil {
			return Result{}, err
		}
		defer tx.Rollback(context.Background())
		claims, err := manager.Transaction(tx)
		if err != nil {
			return Result{}, err
		}
		result, err := claims.Acquire(t.Context(), request)
		if err != nil {
			return Result{}, err
		}
		if result.New {
			if _, err := claims.Complete(t.Context(), result.Claim, []byte(
				`{"recordId":"`+uuid.New().String()+`","sequence":1}`,
			)); err != nil {
				return Result{}, err
			}
		}
		if err := tx.Commit(t.Context()); err != nil {
			return Result{}, err
		}
		return result, nil
	}

	created, err := acquire(`{"b":2,"a":1}`)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := acquire("{\n\"a\":1.0,\"b\":2}")
	if err != nil {
		t.Fatal(err)
	}
	if !created.New || replayed.New || replayed.Claim.ID != created.Claim.ID ||
		replayed.Claim.State != "completed" {
		t.Fatalf("created = %+v replayed = %+v", created, replayed)
	}
	if _, err := acquire(`{"a":1,"b":3}`); err == nil {
		t.Fatal("expected different Actor input to conflict")
	} else {
		var conflict ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("conflict error = %v", err)
		}
	}
}

func TestPostgresLookupHMACAuthorityFencesRotation(t *testing.T) {
	pool := openClaimPostgres(t)
	hashes := claimTestKeys(t)
	authority := keyedhash.NewAuthority(hashes)

	activation, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Activate(t.Context(), activation, 1); err != nil {
		t.Fatal(err)
	}
	if err := activation.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	derivation, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	selection, err := authority.Lock(t.Context(), db.New(derivation))
	if err != nil {
		t.Fatal(err)
	}
	if selection.Current != 1 {
		t.Fatalf("current version = %d, want 1", selection.Current)
	}

	blocked, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocked.Exec(t.Context(), `SET LOCAL lock_timeout = '100ms'`); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Activate(t.Context(), blocked, 2); err == nil {
		t.Fatal("expected activation to wait for the derivation lock")
	}
	if err := blocked.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := derivation.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	activation, err = pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Activate(t.Context(), activation, 2); err != nil {
		t.Fatal(err)
	}
	if err := activation.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	wrongKeys, err := keyedhash.New(map[int32][]byte{
		1: bytes.Repeat([]byte{1}, keyedhash.KeySize),
		2: bytes.Repeat([]byte{9}, keyedhash.KeySize),
	})
	if err != nil {
		t.Fatal(err)
	}
	wrongAuthority := keyedhash.NewAuthority(wrongKeys)
	derivation, err = pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrongAuthority.Lock(t.Context(), db.New(derivation)); err == nil {
		t.Fatal("expected mismatched key bytes to fail closed")
	}
	if err := derivation.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func validatedClaimManager(t *testing.T, current int32, pool *pgxpool.Pool, queries *db.Queries) Manager {
	t.Helper()
	hashes := claimTestKeys(t)
	rows, err := queries.ListLookupHMACVersions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	activeCurrent := int32(0)
	for _, row := range rows {
		if row.IsCurrent {
			activeCurrent = row.Version
		}
	}
	if activeCurrent != current {
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		authority := keyedhash.NewAuthority(hashes)
		if _, err := authority.Activate(t.Context(), tx, current); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	return New(hashes)
}

func claimTestKeys(t *testing.T) keyedhash.Keyring {
	t.Helper()
	hashes, err := keyedhash.New(map[int32][]byte{
		1: bytes.Repeat([]byte{1}, keyedhash.KeySize),
		2: bytes.Repeat([]byte{2}, keyedhash.KeySize),
	})
	if err != nil {
		t.Fatal(err)
	}
	return hashes
}

func secretCreateRequest(t *testing.T, environmentID uuid.UUID, key string, value string) Request {
	t.Helper()
	request, err := NewSecretCreateRequest(environmentID, "API_TOKEN", key, func(int32) ([32]byte, error) {
		return sha256Sum(value), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func sha256Sum(value string) [32]byte {
	return sha256.Sum256([]byte(value))
}

func openClaimPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	for _, name := range []string{"initdb", "pg_ctl", "postgres"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not found; skipping PostgreSQL claim test", name)
		}
	}
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	if output, err := exec.Command("initdb", "-D", dataDir, "-A", "trust").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, output)
	}
	port := freeClaimPostgresPort(t)
	logPath := filepath.Join(tmp, "postgres.log")
	command := exec.Command("pg_ctl", "-D", dataDir, "-l", logPath, "-o", fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1", port), "-w", "start")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command("pg_ctl", "-D", dataDir, "-m", "fast", "-w", "stop").Run()
	})
	dsn := fmt.Sprintf("postgres://%s@127.0.0.1:%d/postgres?sslmode=disable", os.Getenv("USER"), port)
	if err := schema.Up(t.Context(), dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedClaimEnvironment(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	orgID := uuid.Must(uuid.NewV7())
	projectID := uuid.Must(uuid.NewV7())
	environmentID := uuid.Must(uuid.NewV7())
	regionID := "claim-" + environmentID.String()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO organizations (id, public_id, name, slug)
		VALUES ($1, $2, 'Claims', $3)
	`, orgID, publicID("org_", orgID), "claims-"+orgID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO regions (id, provider, provider_region, display_name)
		VALUES ($1, 'test', $1, 'Claims')
	`, regionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO projects (id, public_id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, $3, $4, $5, 'Claims')
	`, projectID, publicID("prj_", projectID), orgID, regionID, "claims-"+projectID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO environments (id, public_id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, 'production', 'Production', '#000000')
	`, environmentID, publicID("env_", environmentID), orgID, projectID); err != nil {
		t.Fatal(err)
	}
	return environmentID
}

func publicID(prefix string, id uuid.UUID) string {
	return prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(id[:]))
}

func freeClaimPostgresPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
