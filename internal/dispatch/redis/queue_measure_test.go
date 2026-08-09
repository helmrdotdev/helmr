package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/dispatch"
	redisv9 "github.com/redis/go-redis/v9"
)

type queueMeasurement struct {
	Rows             int     `json:"rows"`
	Scopes           int     `json:"scopes"`
	EnqueueMillis    float64 `json:"enqueue_millis"`
	SelectionMinimum float64 `json:"selection_minimum_millis"`
	SelectionMedian  float64 `json:"selection_median_millis"`
	SelectionMaximum float64 `json:"selection_maximum_millis"`
	Selected         int     `json:"selected"`
}

func TestMeasureReadyQueue(t *testing.T) {
	if os.Getenv("HELMR_MEASURE_DISPATCH") != "1" {
		t.Skip("HELMR_MEASURE_DISPATCH=1 is required")
	}
	address := os.Getenv("HELMR_TEST_REDIS_ADDR")
	if address == "" {
		t.Skip("HELMR_TEST_REDIS_ADDR is required")
	}

	ctx := context.Background()
	client := redisv9.NewClient(&redisv9.Options{
		Network: os.Getenv("HELMR_TEST_REDIS_NETWORK"),
		Addr:    address,
	})
	t.Cleanup(func() { _ = client.Close() })
	now := time.Date(2026, time.January, 1, 1, 0, 0, 0, time.UTC)
	prefix := fmt.Sprintf("helmr:dispatch-measure:%d", time.Now().UnixNano())
	queue, err := New(client, WithPrefix(prefix), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}

	const rows = 25_600
	const scopes = 256
	start := time.Now()
	for index := range rows {
		scope := index % scopes
		origin := now.Add(time.Duration(index) * time.Millisecond)
		priority := int32(index % 11)
		message := dispatch.Message{
			WorkKind: dispatch.WorkKindRun, RunID: fmt.Sprintf("run-%05d", index),
			OrgID: "org-1", ProjectID: "project-1", EnvironmentID: "environment-1", RegionID: "us-east-1",
			QueueName: fmt.Sprintf("measure-%04d", scope), RunStateVersion: 1, Priority: priority,
			QueueOriginAt: origin, QueueScoreAt: origin.Add(-time.Duration(priority) * time.Second), EnqueuedAt: now,
		}
		if scope%2 != 0 {
			message.ConcurrencyKey = fmt.Sprintf("key-%04d", scope)
		}
		if _, err := queue.Enqueue(ctx, message); err != nil {
			t.Fatal(err)
		}
	}
	enqueueDuration := time.Since(start)

	durations := make([]time.Duration, 0, 12)
	selected := 0
	for iteration := 0; iteration < 15; iteration++ {
		start = time.Now()
		messages, err := queue.SelectReady(ctx, dispatch.ReadySelection{
			WorkKind: dispatch.WorkKindRun, RegionID: "us-east-1", Limit: 32,
			OrganizationScanLimit: 32, EnvironmentScanLimit: 8,
			LeafScanLimit: 32, LeafContributionLimit: 4, TenantContributionLimit: 32,
			OldestWorkAfter: time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		if iteration >= 3 {
			durations = append(durations, time.Since(start))
		}
		selected = len(messages)
	}
	slices.Sort(durations)
	measurement := queueMeasurement{
		Rows: rows, Scopes: scopes, EnqueueMillis: measureMilliseconds(enqueueDuration), Selected: selected,
		SelectionMinimum: measureMilliseconds(durations[0]), SelectionMedian: measureMilliseconds(durations[len(durations)/2]),
		SelectionMaximum: measureMilliseconds(durations[len(durations)-1]),
	}
	payload, err := json.Marshal(measurement)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("dispatch_queue_measurement=%s", payload)
}

func measureMilliseconds(duration time.Duration) float64 {
	return float64(duration.Microseconds()) / 1000
}
