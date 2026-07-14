package fleet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
)

type fakeCloudWatch struct {
	calls  int
	inputs []*cloudwatch.PutMetricDataInput
	err    error
}

func (f *fakeCloudWatch) PutMetricData(_ context.Context, input *cloudwatch.PutMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
	f.calls++
	f.inputs = append(f.inputs, input)
	return &cloudwatch.PutMetricDataOutput{}, f.err
}

func TestCloudWatchPublisherThrottlesAndUsesGroupRoleDimensions(t *testing.T) {
	client := &fakeCloudWatch{}
	publisher, err := NewCloudWatchPublisher(client, "Helmr/Test", "run", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	publisher.now = func() time.Time { return now }
	metric := FleetMetrics{GroupID: "run-workers", Outcome: OutcomeConfirmed, Pending: 2, DrainAge: 3 * time.Second}
	if err := publisher.Publish(context.Background(), metric); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), metric); err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 || len(client.inputs[0].MetricData) != 13 {
		t.Fatalf("calls=%d input=%#v", client.calls, client.inputs)
	}
	for _, datum := range client.inputs[0].MetricData {
		if len(datum.Dimensions) != 2 {
			t.Fatalf("dimensions = %#v", datum.Dimensions)
		}
	}
}

func TestCloudWatchPublisherDefersMutatingPlannedEventAndReturnsDenial(t *testing.T) {
	client := &fakeCloudWatch{err: errors.New("access denied")}
	publisher, err := NewCloudWatchPublisher(client, "Helmr/Test", "build", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	publisher.now = func() time.Time { return testNow }
	metric := FleetMetrics{GroupID: "build-workers", Action: ActionLaunch, Outcome: OutcomePlanned}
	if err := publisher.Publish(context.Background(), metric); err != nil || client.calls != 0 {
		t.Fatalf("planned mutation publish err=%v calls=%d", err, client.calls)
	}
	metric.Outcome = OutcomeRetrying
	if err := publisher.Publish(context.Background(), metric); err == nil || client.calls != 1 {
		t.Fatalf("denial err=%v calls=%d", err, client.calls)
	}
}
