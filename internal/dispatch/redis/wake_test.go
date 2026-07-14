package redis

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	redisv9 "github.com/redis/go-redis/v9"
)

func TestWakePublisherUsesWorkerEpochChannelAndRuntimeFence(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisv9.NewClient(&redisv9.Options{Addr: server.Addr()})
	publisher, err := NewWakePublisher(client)
	if err != nil {
		t.Fatal(err)
	}
	workerID := uuid.Must(uuid.NewV7())
	runtimeID := uuid.Must(uuid.NewV7())
	authorityID := uuid.Must(uuid.NewV7())
	channel := "helmr:worker:" + workerID.String() + ":epoch:9:wake"
	subscription := client.Subscribe(context.Background(), channel)
	defer subscription.Close()
	if _, err := subscription.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	wake := dispatch.WorkerWake{Domain: "checkpoint", WorkerID: pgvalue.UUID(workerID), WorkerEpoch: 9,
		RuntimeID: pgvalue.UUID(runtimeID), AuthorityID: pgvalue.UUID(authorityID), RequestVersion: 3}
	if err := publisher.PublishWorkerWake(context.Background(), wake); err != nil {
		t.Fatal(err)
	}
	message, err := subscription.ReceiveMessage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(message.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["domain"] != "checkpoint" || payload["authority_id"] != authorityID.String() ||
		payload["runtime_instance_id"] != runtimeID.String() || payload["worker_epoch"] != float64(9) ||
		payload["request_version"] != float64(3) {
		t.Fatalf("wake payload = %#v", payload)
	}
	if err := publisher.PublishWorkerWake(context.Background(), wake); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := subscription.ReceiveMessage(ctx); err != nil {
		t.Fatal(err)
	}
}
