package dbtest

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const databaseURLEnvironment = "HELMR_TEST_DATABASE_URL"

type Database struct {
	DSN  string
	Pool *pgxpool.Pool
}

func Open(t *testing.T) Database {
	t.Helper()
	if dsn := strings.TrimSpace(os.Getenv(databaseURLEnvironment)); dsn != "" {
		return openIsolatedDatabase(t, dsn)
	}
	return openLocalCluster(t)
}

func openIsolatedDatabase(t *testing.T, dsn string) Database {
	t.Helper()
	admin, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Ping(t.Context()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	name := "helmr_test_" + strings.ReplaceAll(uuid.NewString(), "-", "_")
	if _, err := admin.Exec(
		t.Context(),
		"CREATE DATABASE "+pgx.Identifier{name}.Sanitize(),
	); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(
			context.Background(),
			"DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)",
		)
		admin.Close()
	})
	testDSN := databaseDSN(t, dsn, name)
	pool, err := pgxpool.New(t.Context(), testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return Database{DSN: testDSN, Pool: pool}
}

func openLocalCluster(t *testing.T) Database {
	t.Helper()
	for _, name := range []string{"initdb", "pg_ctl", "postgres"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not found; skipping PostgreSQL test", name)
		}
	}
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if output, err := exec.Command(
		"initdb",
		"-D",
		dataDir,
		"-A",
		"trust",
	).CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, output)
	}
	port := freePort(t)
	logPath := filepath.Join(root, "postgres.log")
	if output, err := exec.Command(
		"pg_ctl",
		"-D",
		dataDir,
		"-l",
		logPath,
		"-o",
		fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1", port),
		"-w",
		"start",
	).CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command(
			"pg_ctl",
			"-D",
			dataDir,
			"-m",
			"fast",
			"-w",
			"stop",
		).Run()
	})
	dsn := fmt.Sprintf(
		"postgres://%s@127.0.0.1:%d/postgres?sslmode=disable",
		os.Getenv("USER"),
		port,
	)
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return Database{DSN: dsn, Pool: pool}
}

func databaseDSN(t *testing.T, dsn string, database string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + database
	return parsed.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
