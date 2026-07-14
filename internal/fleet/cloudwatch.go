package fleet

import (
	"context"
	"errors"
	"sync"
	"time"

	awsdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

type CloudWatchAPI interface {
	PutMetricData(context.Context, *cloudwatch.PutMetricDataInput, ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error)
}

// CloudWatchPublisher is projection-only. The controller ignores its errors,
// and this publisher enforces a per-fleet interval to bound custom metric API
// traffic. Mutating cycles publish the terminal confirmed/retrying outcome,
// not their preceding planned event.
type CloudWatchPublisher struct {
	client    CloudWatchAPI
	namespace string
	role      string
	interval  time.Duration
	now       func() time.Time

	mu   sync.Mutex
	last time.Time
}

func NewCloudWatchPublisher(client CloudWatchAPI, namespace, role string, interval time.Duration) (*CloudWatchPublisher, error) {
	if client == nil || namespace == "" || (role != "run" && role != "build") || interval <= 0 {
		return nil, errors.New("cloudwatch publisher requires client, namespace, run/build role, and positive interval")
	}
	return &CloudWatchPublisher{client: client, namespace: namespace, role: role, interval: interval, now: time.Now}, nil
}

func (p *CloudWatchPublisher) Publish(ctx context.Context, metric FleetMetrics) error {
	if metric.Outcome == OutcomePlanned && metric.Action != ActionNone {
		return nil
	}
	now := p.now()
	p.mu.Lock()
	if !p.last.IsZero() && now.Sub(p.last) < p.interval {
		p.mu.Unlock()
		return nil
	}
	p.last = now
	p.mu.Unlock()

	dimensions := []types.Dimension{
		{Name: awsdk.String("WorkerGroupID"), Value: awsdk.String(metric.GroupID)},
		{Name: awsdk.String("Role"), Value: awsdk.String(p.role)},
	}
	outcome := 0.0
	if metric.Outcome == OutcomeConfirmed {
		outcome = 1
	} else if metric.Outcome == OutcomeRetrying {
		outcome = 2
	}
	values := []struct {
		name  string
		value float64
		unit  types.StandardUnit
	}{
		{"UncappedRequiredWorkers", float64(metric.UncappedRequired), types.StandardUnitCount},
		{"UnmetCapacity", float64(metric.UnmetDeficit), types.StandardUnitCount},
		{"DesiredWorkers", float64(metric.Desired), types.StandardUnitCount},
		{"SupplyWorkers", float64(metric.Supply), types.StandardUnitCount},
		{"PendingWorkers", float64(metric.Pending), types.StandardUnitCount},
		{"BillableWorkers", float64(metric.Billable), types.StandardUnitCount},
		{"UncertifiedRunLaunchAttestations", float64(metric.UncertifiedRunLaunchAttestations), types.StandardUnitCount},
		{"BootstrapPending", boolFloat(metric.BootstrapPending), types.StandardUnitCount},
		{"DrainAgeSeconds", metric.DrainAge.Seconds(), types.StandardUnitSeconds},
		{"OldestQueueAgeSeconds", metric.QueueAge.Seconds(), types.StandardUnitSeconds},
		{"DrainTimedOut", boolFloat(metric.DrainTimedOut), types.StandardUnitCount},
		{"ControllerAction", float64(metric.Action), types.StandardUnitCount},
		{"ControllerOutcome", outcome, types.StandardUnitCount},
	}
	data := make([]types.MetricDatum, 0, len(values))
	for _, value := range values {
		data = append(data, types.MetricDatum{
			MetricName: awsdk.String(value.name), Value: awsdk.Float64(value.value), Unit: value.unit,
			Timestamp: awsdk.Time(now), Dimensions: dimensions,
		})
	}
	_, err := p.client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{Namespace: awsdk.String(p.namespace), MetricData: data})
	return err
}

func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
