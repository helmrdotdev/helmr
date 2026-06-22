package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

const (
	workspaceStreamWakeupBatchSize     = int32(100)
	workspaceStreamWakeupLeaseDuration = 30 * time.Second
	workspaceStreamWakeupIdleEvery     = 100 * time.Millisecond
	workspaceStreamWakeupMaxAttempts   = int32(25)
	workspaceStreamWakeupMaxLen        = int64(10000)
	workspaceStreamWakeupBlockEvery    = 25 * time.Second
	workspaceStreamFollowMaxDuration   = 30 * time.Minute
	workspaceStreamWaitRetryEvery      = 250 * time.Millisecond
)

type WorkspaceStreamNotifier struct {
	log   *slog.Logger
	db    db.Querier
	redis redis.Cmdable
}

type workspaceStreamWakeup struct {
	ID               int64  `json:"id"`
	OrgID            string `json:"org_id"`
	WorkspaceID      string `json:"workspace_id"`
	ResourceKind     string `json:"resource_kind"`
	ResourceID       string `json:"resource_id"`
	Stream           string `json:"stream"`
	CursorOffset     int64  `json:"cursor_offset"`
	NotificationKind string `json:"notification_kind"`
}

func NewWorkspaceStreamNotifier(log *slog.Logger, queries db.Querier, redis redis.Cmdable) (*WorkspaceStreamNotifier, error) {
	if queries == nil {
		return nil, errors.New("workspace stream notifier database is required")
	}
	if redis == nil {
		return nil, errors.New("workspace stream notifier redis client is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &WorkspaceStreamNotifier{log: log, db: queries, redis: redis}, nil
}

func (n *WorkspaceStreamNotifier) RunPublisher(ctx context.Context) error {
	consecutiveFailures := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		claimed, err := n.db.ClaimWorkspaceStreamWakeups(ctx, db.ClaimWorkspaceStreamWakeupsParams{
			MaxAttempts:   workspaceStreamWakeupMaxAttempts,
			RowLimit:      workspaceStreamWakeupBatchSize,
			LeaseDuration: pgvalue.Interval(workspaceStreamWakeupLeaseDuration),
		})
		if err != nil {
			consecutiveFailures++
			n.log.Warn("claim workspace stream wakeups failed", "error", err)
			if sleepErr := sleepWithContext(ctx, eventPublisherBackoff(consecutiveFailures)); sleepErr != nil {
				return sleepErr
			}
			continue
		}
		consecutiveFailures = 0
		if len(claimed) == 0 {
			if err := sleepWithContext(ctx, workspaceStreamWakeupIdleEvery); err != nil {
				return err
			}
			continue
		}
		for _, row := range claimed {
			if err := n.publishWakeup(ctx, row); err != nil {
				n.log.Warn("publish workspace stream wakeup failed", "wakeup_id", row.ID, "error", err)
				if markErr := n.db.MarkWorkspaceStreamWakeupFailed(ctx, db.MarkWorkspaceStreamWakeupFailedParams{
					ID:          row.ID,
					LastError:   err.Error(),
					RetryAfter:  pgvalue.Interval(eventPublisherBackoff(int(row.Attempts))),
					MaxAttempts: workspaceStreamWakeupMaxAttempts,
				}); markErr != nil {
					n.log.Warn("mark workspace stream wakeup failed", "wakeup_id", row.ID, "error", markErr)
					if sleepErr := sleepWithContext(ctx, eventPublisherBackoff(int(row.Attempts))); sleepErr != nil {
						return sleepErr
					}
				}
				continue
			}
			if err := n.db.DeleteWorkspaceStreamWakeup(ctx, row.ID); err != nil {
				n.log.Warn("delete published workspace stream wakeup failed", "wakeup_id", row.ID, "error", err)
				if sleepErr := sleepWithContext(ctx, eventPublisherBackoff(int(row.Attempts))); sleepErr != nil {
					return sleepErr
				}
			}
		}
	}
}

func (n *WorkspaceStreamNotifier) publishWakeup(ctx context.Context, row db.ClaimWorkspaceStreamWakeupsRow) error {
	payload, err := json.Marshal(workspaceStreamWakeup{
		ID:               row.ID,
		OrgID:            pgvalue.MustUUIDValue(row.OrgID).String(),
		WorkspaceID:      pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		ResourceKind:     string(row.ResourceKind),
		ResourceID:       pgvalue.MustUUIDValue(row.ResourceID).String(),
		Stream:           row.Stream,
		CursorOffset:     row.CursorOffset,
		NotificationKind: string(row.NotificationKind),
	})
	if err != nil {
		return fmt.Errorf("encode workspace stream wakeup: %w", err)
	}
	id := redisEventID(row.ID)
	err = n.redis.XAdd(ctx, &redis.XAddArgs{
		Stream: workspaceStreamKey(row.OrgID, string(row.ResourceKind), row.ResourceID, row.Stream),
		MaxLen: workspaceStreamWakeupMaxLen,
		Approx: true,
		ID:     id,
		Values: map[string]any{"wakeup": string(payload)},
	}).Err()
	if err == nil || redisIDAlreadyExists(err) {
		return nil
	}
	return err
}

func (n *WorkspaceStreamNotifier) LatestID(ctx context.Context, streamKey string) (string, error) {
	records, err := n.redis.XRevRangeN(ctx, streamKey, "+", "-", 1).Result()
	if errors.Is(err, redis.Nil) {
		return "0-0", nil
	}
	if err != nil {
		return "", err
	}
	if len(records) == 0 {
		return "0-0", nil
	}
	return records[0].ID, nil
}

func (n *WorkspaceStreamNotifier) Wait(ctx context.Context, streamKey string, cursor string) (string, error) {
	deadline := time.Now().Add(workspaceStreamWakeupBlockEvery)
	next := cursor
	for {
		blockFor := time.Until(deadline)
		if blockFor <= 0 {
			return next, nil
		}
		streams, err := n.redis.XRead(ctx, &redis.XReadArgs{
			Streams: []string{streamKey, next},
			Count:   int64(workspaceStreamWakeupBatchSize),
			Block:   blockFor,
		}).Result()
		if errors.Is(err, redis.Nil) {
			return next, nil
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return next, ctxErr
			}
			n.log.Warn("wait workspace stream wakeup read failed", "stream_key", streamKey, "error", err)
			if sleepErr := sleepWithContext(ctx, workspaceStreamWaitRetryEvery); sleepErr != nil {
				return next, sleepErr
			}
			continue
		}
		for _, stream := range streams {
			for _, message := range stream.Messages {
				next = message.ID
			}
		}
		if next != cursor {
			return next, nil
		}
	}
}

func workspaceStreamKey(orgID pgtype.UUID, resourceKind string, resourceID pgtype.UUID, stream string) string {
	return "helmr:workspace-streams:" + pgvalue.MustUUIDValue(orgID).String() + ":" + resourceKind + ":" + pgvalue.MustUUIDValue(resourceID).String() + ":" + stream
}
