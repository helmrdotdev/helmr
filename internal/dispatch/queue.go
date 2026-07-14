package dispatch

import (
	"context"
	"time"
)

type EnqueueResult struct {
	QueueName string
	MessageID string
	Depth     int64
}

type ReadySelection struct {
	WorkKind                WorkKind
	RegionID                string
	Limit                   int
	OrganizationScanLimit   int64
	EnvironmentScanLimit    int64
	LeafScanLimit           int64
	LeafContributionLimit   int64
	TenantContributionLimit int
	OldestWorkAfter         time.Duration
}

type Queue interface {
	Enqueue(context.Context, Message) (EnqueueResult, error)
	ReadyRegions(context.Context, WorkKind, int64) ([]string, error)
	SelectReady(context.Context, ReadySelection) ([]Message, error)
	RemoveReady(context.Context, WorkKind, string, string) error
}
