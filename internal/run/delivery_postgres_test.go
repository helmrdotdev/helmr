package run

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRunAdmissionOutboxClaimsAreTopicIsolatedAndFenced(t *testing.T) {
	pool := openDeliveryPostgres(t)
	queries := db.New(pool)
	runAdmissionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	secretRevocationID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	for _, value := range []db.CreateOutboxMessageParams{
		{
			ID:           runAdmissionID,
			Lane:         "control",
			Topic:        "run.admit",
			PartitionKey: "workspace",
			Payload:      []byte(`{"environmentId":"00000000-0000-0000-0000-000000000001","runId":"00000000-0000-0000-0000-000000000002"}`),
			AvailableAt:  pgvalue.TimestamptzUTCZeroInvalid(time.Now().Add(-time.Minute)),
		},
		{
			ID:           secretRevocationID,
			Lane:         "control",
			Topic:        "secret.revoked",
			PartitionKey: "secret",
			Payload:      []byte(`{"secretId":"00000000-0000-0000-0000-000000000003"}`),
			AvailableAt:  pgvalue.TimestamptzUTCZeroInvalid(time.Now().Add(-time.Minute)),
		},
	} {
		if _, err := queries.CreateOutboxMessage(t.Context(), value); err != nil {
			t.Fatal(err)
		}
	}

	first := claimRunAdmissionMessages(t, queries, "worker-a")
	if len(first) != 1 || pgvalue.UUIDString(first[0].ID) != pgvalue.UUIDString(runAdmissionID) {
		t.Fatalf("first claim = %+v", first)
	}
	var secretState string
	if err := pool.QueryRow(t.Context(), `SELECT state FROM outbox_messages WHERE id = $1`, secretRevocationID).Scan(&secretState); err != nil {
		t.Fatal(err)
	}
	if secretState != "pending" {
		t.Fatalf("secret.revoked state = %q", secretState)
	}

	if _, err := pool.Exec(t.Context(), `
		UPDATE outbox_messages
		   SET claim_expires_at = now() - interval '1 second'
		 WHERE id = $1
	`, runAdmissionID); err != nil {
		t.Fatal(err)
	}
	second := claimRunAdmissionMessages(t, queries, "worker-b")
	if len(second) != 1 || second[0].Attempts != 2 {
		t.Fatalf("second claim = %+v", second)
	}

	stale := db.DeliverOutboxMessageParams{
		ID:           first[0].ID,
		ClaimedBy:    first[0].ClaimedBy,
		ClaimAttempt: first[0].Attempts,
	}
	if _, err := queries.DeliverOutboxMessage(t.Context(), stale); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale delivery error = %v", err)
	}
	if _, err := queries.RetryOutboxMessage(t.Context(), db.RetryOutboxMessageParams{
		ID:           stale.ID,
		ClaimedBy:    stale.ClaimedBy,
		ClaimAttempt: stale.ClaimAttempt,
		AvailableAt:  pgvalue.TimestamptzUTCZeroInvalid(time.Now()),
		LastError:    pgvalue.Text("stale"),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale retry error = %v", err)
	}
	if _, err := queries.DeadLetterOutboxMessage(t.Context(), db.DeadLetterOutboxMessageParams{
		ID:           stale.ID,
		ClaimedBy:    stale.ClaimedBy,
		ClaimAttempt: stale.ClaimAttempt,
		LastError:    pgvalue.Text("stale"),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale dead-letter error = %v", err)
	}
	if _, err := queries.DeliverOutboxMessage(t.Context(), db.DeliverOutboxMessageParams{
		ID:           second[0].ID,
		ClaimedBy:    second[0].ClaimedBy,
		ClaimAttempt: second[0].Attempts,
	}); err != nil {
		t.Fatal(err)
	}

	var indexDefinition string
	if err := pool.QueryRow(t.Context(), `
		SELECT pg_get_indexdef(indexrelid)
		  FROM pg_index
		 WHERE indexrelid = 'outbox_messages_delivery_idx'::regclass
	`).Scan(&indexDefinition); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(indexDefinition, "(lane, topic, available_at, id)") {
		t.Fatalf("delivery index = %q", indexDefinition)
	}
}

func claimRunAdmissionMessages(t *testing.T, queries *db.Queries, worker string) []db.OutboxMessage {
	t.Helper()
	messages, err := queries.ClaimOutboxMessages(t.Context(), db.ClaimOutboxMessagesParams{
		ClaimedBy:      pgtype.Text{String: worker, Valid: true},
		ClaimExpiresAt: pgvalue.TimestamptzUTCZeroInvalid(time.Now().Add(time.Hour)),
		Lane:           "control",
		Topics:         []string{"run.admit"},
		RowLimit:       10,
	})
	if err != nil {
		t.Fatal(err)
	}
	return messages
}

func openDeliveryPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	for _, name := range []string{"initdb", "pg_ctl", "postgres"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not found; skipping PostgreSQL Run admission delivery test", name)
		}
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	if output, err := exec.Command("initdb", "-D", dataDir, "-A", "trust").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, output)
	}
	port := freeDeliveryPostgresPort(t)
	logPath := filepath.Join(filepath.Dir(dataDir), "postgres.log")
	command := exec.Command(
		"pg_ctl",
		"-D",
		dataDir,
		"-l",
		logPath,
		"-o",
		fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1", port),
		"-w",
		"start",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command("pg_ctl", "-D", dataDir, "-m", "fast", "-w", "stop").Run()
	})
	dsn := fmt.Sprintf(
		"postgres://%s@127.0.0.1:%d/postgres?sslmode=disable",
		os.Getenv("USER"),
		port,
	)
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

func freeDeliveryPostgresPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
