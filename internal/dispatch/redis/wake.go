package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	redisv9 "github.com/redis/go-redis/v9"
)

type WakePublisher struct {
	client redisv9.UniversalClient
}

func NewWakePublisher(client redisv9.UniversalClient) (*WakePublisher, error) {
	if client == nil {
		return nil, fmt.Errorf("wake publisher redis client is required")
	}
	return &WakePublisher{client: client}, nil
}

func (p *WakePublisher) PublishWorkerWake(ctx context.Context, wake dispatch.WorkerWake) error {
	workerID := pgvalue.UUIDString(wake.WorkerID)
	authorityID := pgvalue.UUIDString(wake.AuthorityID)
	if workerID == "" || authorityID == "" || wake.WorkerEpoch <= 0 || wake.Domain == "" {
		return fmt.Errorf("invalid worker wake")
	}
	runtimeID := pgvalue.UUIDString(wake.RuntimeID)
	if (wake.Domain == "run" || wake.Domain == "runtime" || wake.Domain == "checkpoint") && runtimeID == "" {
		return fmt.Errorf("invalid worker wake: runtime fence is required for %s", wake.Domain)
	}
	if wake.Domain == "checkpoint" && wake.RequestVersion <= 0 {
		return fmt.Errorf("invalid worker wake: checkpoint request version is required")
	}
	body := map[string]any{
		"domain": wake.Domain, "authority_id": authorityID,
		"worker_epoch": wake.WorkerEpoch,
	}
	if runtimeID != "" {
		body["runtime_instance_id"] = runtimeID
	}
	if wake.RequestVersion > 0 {
		body["request_version"] = wake.RequestVersion
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode worker wake: %w", err)
	}
	channel := "helmr:worker:" + workerID + ":epoch:" + strconv.FormatInt(wake.WorkerEpoch, 10) + ":wake"
	if err := p.client.Publish(ctx, channel, payload).Err(); err != nil {
		return fmt.Errorf("publish worker wake: %w", err)
	}
	return nil
}
