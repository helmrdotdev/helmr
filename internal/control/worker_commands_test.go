package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

func TestWorkerCommandStreamDeliversWorkerScopedCommand(t *testing.T) {
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })

	workerInstanceID := uuid.MustParse("019f0d01-0000-7000-8000-000000000001")
	row := db.ClaimWorkerCommandsRow{
		ID:               42,
		OrgID:            pgvalue.UUID(uuid.MustParse("019f0d01-0000-7000-8000-000000000002")),
		ProjectID:        pgvalue.UUID(uuid.MustParse("019f0d01-0000-7000-8000-000000000003")),
		EnvironmentID:    pgvalue.UUID(uuid.MustParse("019f0d01-0000-7000-8000-000000000004")),
		RunID:            pgvalue.UUID(uuid.MustParse("019f0d01-0000-7000-8000-000000000005")),
		RunWaitID:        pgvalue.UUID(uuid.MustParse("019f0d01-0000-7000-8000-000000000006")),
		RunLeaseID:       pgvalue.UUID(uuid.MustParse("019f0d01-0000-7000-8000-000000000007")),
		WorkerInstanceID: pgvalue.UUID(workerInstanceID),
		RunStateVersion:  pgtype.Int8{Int64: 3, Valid: true},
		Kind:             db.WorkerCommandKindRunResumeWait,
		Payload:          []byte(`{"kind":"completed"}`),
	}
	stream := &WorkerCommandStream{
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		redis: redisClient,
	}

	if err := stream.deliverCommand(context.Background(), row); err != nil {
		t.Fatal(err)
	}

	messages, err := redisClient.XRange(context.Background(), workerCommandStreamKey(pgvalue.UUID(workerInstanceID)), "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != "42-0" {
		t.Fatalf("messages = %+v", messages)
	}
	raw, ok := messages[0].Values["command"].(string)
	if !ok {
		t.Fatalf("command payload = %#v", messages[0].Values["command"])
	}
	var payload workerCommand
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ID != 42 || payload.WorkerInstanceID != workerInstanceID.String() || payload.Kind != string(db.WorkerCommandKindRunResumeWait) || payload.RunStateVersion != 3 {
		t.Fatalf("payload = %+v", payload)
	}
	if string(payload.Payload) != `{"kind":"completed"}` {
		t.Fatalf("payload = %s", payload.Payload)
	}
}

func TestAdvanceWorkerCommandRedisCursor(t *testing.T) {
	tests := []struct {
		name    string
		cursor  string
		afterID int64
		want    string
		wantErr bool
	}{
		{name: "no replay", cursor: "7-0", afterID: 0, want: "7-0"},
		{name: "advance after replay", cursor: "0-0", afterID: 42, want: "42-0"},
		{name: "do not move backwards", cursor: "99-0", afterID: 42, want: "99-0"},
		{name: "invalid cursor", cursor: "invalid", afterID: 42, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := advanceWorkerCommandRedisCursor(tt.cursor, tt.afterID)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("cursor = %q, want %q", got, tt.want)
			}
		})
	}
}
