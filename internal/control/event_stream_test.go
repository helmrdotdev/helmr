package control

import (
	"io"
	"log/slog"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestEventStream(t *testing.T) *EventStream {
	t.Helper()
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	return &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), redis: redisClient, telemetryReader: fakeTelemetryReader{store: &fakeStore{}}}
}
