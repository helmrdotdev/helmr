package dispatch

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestQueueScopeLockKey(t *testing.T) {
	environmentID := pgtype.UUID{
		Bytes: [16]byte{0x01, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45},
		Valid: true,
	}
	tests := []struct {
		name           string
		queue          string
		concurrencyKey pgtype.Text
		want           int64
	}{
		{name: "null concurrency", queue: "default", want: 7913748723398249424},
		{name: "explicit concurrency", queue: "agents", concurrencyKey: pgtype.Text{String: "repo:helmr", Valid: true}, want: -582241612760218975},
		{name: "unicode bytes", queue: "実行", concurrencyKey: pgtype.Text{String: "顧客:a", Valid: true}, want: 1580311095981330178},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := queueScopeLockKey(environmentID, tt.queue, tt.concurrencyKey)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("queueScopeLockKey() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestQueueScopeLockKeyRejectsInvalidScope(t *testing.T) {
	validID := pgtype.UUID{Valid: true}
	tests := []struct {
		name           string
		environmentID  pgtype.UUID
		queue          string
		concurrencyKey pgtype.Text
	}{
		{name: "missing environment", queue: "default"},
		{name: "blank queue", environmentID: validID, queue: " "},
		{name: "empty persisted concurrency", environmentID: validID, queue: "default", concurrencyKey: pgtype.Text{Valid: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := queueScopeLockKey(tt.environmentID, tt.queue, tt.concurrencyKey); err == nil {
				t.Fatal("queueScopeLockKey() error = nil")
			}
		})
	}
}
