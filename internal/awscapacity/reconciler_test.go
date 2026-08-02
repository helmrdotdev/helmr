package awscapacity

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	awsdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/helmrdotdev/helmr/internal/api"
)

func TestReconcilerScalesOutFromFreshDemand(t *testing.T) {
	now := time.Now().UTC()
	control := &fakeControl{observations: capacityObservations(now, 2)}
	provider := &fakeAutoScaling{group: providerGroup(0)}
	reconciler := testReconciler(t, control, provider, now)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.setDesired == nil || awsdk.ToInt32(provider.setDesired.DesiredCapacity) != 2 {
		t.Fatalf("set desired = %+v", provider.setDesired)
	}
	if control.drainRequest != nil || provider.terminate != nil {
		t.Fatalf("unexpected scale-in actions drain=%+v terminate=%+v", control.drainRequest, provider.terminate)
	}
}

func TestReconcilerCountsDesiredButNotReadyCapacityWithoutOverscaling(t *testing.T) {
	now := time.Now().UTC()
	control := &fakeControl{observations: capacityObservations(now, 2)}
	provider := &fakeAutoScaling{group: providerGroup(2)}
	reconciler := testReconciler(t, control, provider, now)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.setDesired != nil {
		t.Fatalf("set desired = %+v, want no duplicate scale-out", provider.setDesired)
	}
}

func TestReconcilerJointlyAccountsSharedResourcesForDualRoleHost(t *testing.T) {
	now := time.Now().UTC()
	observations := capacityObservations(now, 1)
	observations.Observations[0].Build = &api.OperatorRoleDemand{
		QueuedItems: 2,
		QueuedResources: api.OperatorResourceVector{
			CPUMillis: 4000, MemoryBytes: 8 << 30, GuestEphemeralDiskBytes: 64 << 30, BuildExecutors: 2,
		},
	}
	control := &fakeControl{observations: observations}
	provider := &fakeAutoScaling{group: providerGroup(0)}
	config := Config{
		ObservationMaxAge: time.Minute,
		Groups: []GroupConfig{{
			WorkerGroupID: "run-workers", AutoScalingGroupName: "run-asg",
			TerminationLifecycleHookName: "run-terminate",
			AllowsRun:                    true, AllowsBuild: true,
			InstanceCapacity: api.OperatorResourceVector{
				CPUMillis: 2000, MemoryBytes: 4 << 30, GuestEphemeralDiskBytes: 32 << 30,
				VMSlots: 1, RunConsumers: 1, BuildExecutors: 1,
			},
		}},
	}
	reconciler, err := NewReconciler(nil, control, provider, config)
	if err != nil {
		t.Fatal(err)
	}
	reconciler.now = func() time.Time { return now }
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.setDesired == nil || awsdk.ToInt32(provider.setDesired.DesiredCapacity) != 3 {
		t.Fatalf("set desired = %+v, want joint shared-resource shortage of 3 hosts", provider.setDesired)
	}
}

func TestReconcilerIncludesRunConsumerCapacity(t *testing.T) {
	now := time.Now().UTC()
	observations := capacityObservations(now, 2)
	observations.Observations[0].Run.QueuedResources.CPUMillis = 1
	observations.Observations[0].Run.QueuedResources.MemoryBytes = 1
	observations.Observations[0].Run.QueuedResources.GuestEphemeralDiskBytes = 1
	observations.Observations[0].Run.QueuedResources.VMSlots = 1
	control := &fakeControl{observations: observations}
	provider := &fakeAutoScaling{group: providerGroup(0)}
	reconciler := testReconciler(t, control, provider, now)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.setDesired == nil || awsdk.ToInt32(provider.setDesired.DesiredCapacity) != 2 {
		t.Fatalf("set desired = %+v, want two Run-consumer hosts", provider.setDesired)
	}
}

func TestReconcilerAcceptsDisabledZeroCapacityGroup(t *testing.T) {
	now := time.Now().UTC()
	control := &fakeControl{}
	provider := &fakeAutoScaling{group: providerGroupWithBounds(0, 0, 0)}
	reconciler := testReconciler(t, control, provider, now)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.setDesired != nil || provider.terminate != nil {
		t.Fatalf("disabled group mutated: desired=%+v terminate=%+v", provider.setDesired, provider.terminate)
	}
}

func TestReconcilerBatchesOnlyCurrentProviderLocators(t *testing.T) {
	now := time.Now().UTC()
	control := &fakeControl{observations: capacityObservations(now, 0)}
	instances := make([]autoscalingtypes.Instance, 0, 501)
	for index := 0; index < 501; index++ {
		instances = append(instances, providerInstance(fmt.Sprintf("i-%017x", index+1), "InService"))
	}
	group := providerGroupWithBounds(0, 600, 501)
	group.Instances = instances
	provider := &fakeAutoScaling{group: group}
	reconciler := testReconciler(t, control, provider, now)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(control.instanceQueries) != 3 {
		t.Fatalf("instance query batches = %d, want 3", len(control.instanceQueries))
	}
	for _, query := range control.instanceQueries {
		if len(query) == 0 || len(query) > 200 {
			t.Fatalf("resource locator batch size = %d", len(query))
		}
	}
}

func TestReconcilerStartsExactDrainBeforeScaleIn(t *testing.T) {
	now := time.Now().UTC()
	worker := api.OperatorWorkerInstance{
		ID: "01984b4c-7c5e-7b7c-8e9f-a1b2c3d4e5f6", ResourceID: "i-0123456789abcdef0",
		WorkerGroupID: "run-workers", State: "active", ClaimVersion: 5,
		CurrentEpoch: ptr(int64(3)), CreatedAt: now.Add(-time.Hour),
	}
	control := &fakeControl{
		observations: capacityObservations(now, 0),
		instances:    api.OperatorWorkerInstancesResponse{WorkerInstances: []api.OperatorWorkerInstance{worker}},
	}
	control.observations.Observations[0].Run.ReadyWorkers = 1
	provider := &fakeAutoScaling{group: providerGroup(1, providerInstance(worker.ResourceID, "InService"))}
	reconciler := testReconciler(t, control, provider, now)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if control.drainedID != worker.ID || control.drainRequest == nil || control.drainRequest.ExpectedEpoch != 3 || control.drainRequest.ExpectedClaimVersion != 5 {
		t.Fatalf("drain = id=%q request=%+v", control.drainedID, control.drainRequest)
	}
	if provider.terminate != nil {
		t.Fatal("physical termination occurred before termination_ready")
	}
}

func TestReconcilerTerminatesOnlyTerminationReadyExactHost(t *testing.T) {
	now := time.Now().UTC()
	worker := api.OperatorWorkerInstance{
		ID: "01984b4c-7c5e-7b7c-8e9f-a1b2c3d4e5f6", ResourceID: "i-0123456789abcdef0",
		WorkerGroupID: "run-workers", State: "termination_ready", ClaimVersion: 6,
		CurrentEpoch: ptr(int64(3)), TerminationReadyAt: &now,
	}
	control := &fakeControl{
		observations: capacityObservations(now, 0),
		instances:    api.OperatorWorkerInstancesResponse{WorkerInstances: []api.OperatorWorkerInstance{worker}},
	}
	provider := &fakeAutoScaling{group: providerGroup(1, providerInstance(worker.ResourceID, "InService"))}
	reconciler := testReconciler(t, control, provider, now)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.terminate == nil || awsdk.ToString(provider.terminate.InstanceId) != worker.ResourceID || !awsdk.ToBool(provider.terminate.ShouldDecrementDesiredCapacity) {
		t.Fatalf("termination = %+v", provider.terminate)
	}
}

func TestReconcilerFinishesTerminationReadyWhenGroupObservationIsMissing(t *testing.T) {
	now := time.Now().UTC()
	worker := api.OperatorWorkerInstance{
		ID: "01984b4c-7c5e-7b7c-8e9f-a1b2c3d4e5f6", ResourceID: "i-0123456789abcdef0",
		WorkerGroupID: "run-workers", State: "termination_ready", ClaimVersion: 6,
		CurrentEpoch: ptr(int64(3)), TerminationReadyAt: &now,
	}
	control := &fakeControl{instances: api.OperatorWorkerInstancesResponse{WorkerInstances: []api.OperatorWorkerInstance{worker}}}
	provider := &fakeAutoScaling{group: providerGroup(1, providerInstance(worker.ResourceID, "InService"))}
	reconciler := testReconciler(t, control, provider, now)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.terminate == nil || awsdk.ToString(provider.terminate.InstanceId) != worker.ResourceID {
		t.Fatalf("termination = %+v", provider.terminate)
	}
}

func TestReconcilerRejectsNewCapacityDecisionWhenGroupObservationIsMissing(t *testing.T) {
	now := time.Now().UTC()
	control := &fakeControl{}
	provider := &fakeAutoScaling{group: providerGroup(0)}
	reconciler := testReconciler(t, control, provider, now)
	if err := reconciler.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile succeeded without a group observation")
	}
	if provider.setDesired != nil || provider.terminate != nil {
		t.Fatalf("capacity mutated with missing observation: desired=%+v terminate=%+v", provider.setDesired, provider.terminate)
	}
}

func TestReconcilerFailsClosedBeforeAWSWhenControlUnavailable(t *testing.T) {
	now := time.Now().UTC()
	control := &fakeControl{observationErr: errors.New("unavailable")}
	provider := &fakeAutoScaling{group: providerGroup(1)}
	reconciler := testReconciler(t, control, provider, now)
	if err := reconciler.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile succeeded")
	}
	if provider.describeCalls != 0 || provider.setDesired != nil || provider.terminate != nil {
		t.Fatal("AWS was mutated or inspected after Control failure")
	}
}

func testReconciler(t *testing.T, control Control, provider AutoScaling, now time.Time) *Reconciler {
	t.Helper()
	config := Config{
		ObservationMaxAge: time.Minute,
		Groups: []GroupConfig{{
			WorkerGroupID: "run-workers", AutoScalingGroupName: "run-asg",
			TerminationLifecycleHookName: "run-terminate",
			AllowsRun:                    true,
			InstanceCapacity: api.OperatorResourceVector{
				CPUMillis: 2000, MemoryBytes: 4 << 30, GuestEphemeralDiskBytes: 32 << 30,
				VMSlots: 1, RunConsumers: 1,
			},
		}},
	}
	reconciler, err := NewReconciler(nil, control, provider, config)
	if err != nil {
		t.Fatal(err)
	}
	reconciler.now = func() time.Time { return now }
	return reconciler
}

func capacityObservations(now time.Time, queued int64) api.OperatorCapacityObservationsResponse {
	return api.OperatorCapacityObservationsResponse{Observations: []api.OperatorCapacityObservation{{
		WorkerGroupID: "run-workers", RegionID: "us-east-1", GroupState: "active", ObservedAt: now,
		Run: &api.OperatorRoleDemand{
			QueuedItems: queued,
			QueuedResources: api.OperatorResourceVector{
				CPUMillis: queued * 2000, MemoryBytes: queued * (4 << 30),
				GuestEphemeralDiskBytes: queued * (32 << 30), VMSlots: queued, RunConsumers: queued,
			},
		},
	}}}
}

func providerGroup(desired int32, instances ...autoscalingtypes.Instance) autoscalingtypes.AutoScalingGroup {
	group := providerGroupWithBounds(0, 3, desired)
	group.Instances = instances
	return group
}

func providerGroupWithBounds(minimum, maximum, desired int32) autoscalingtypes.AutoScalingGroup {
	return autoscalingtypes.AutoScalingGroup{
		AutoScalingGroupName: awsdk.String("run-asg"), MinSize: awsdk.Int32(minimum), MaxSize: awsdk.Int32(maximum),
		DesiredCapacity: awsdk.Int32(desired),
	}
}

func providerInstance(id, lifecycle string) autoscalingtypes.Instance {
	return autoscalingtypes.Instance{InstanceId: awsdk.String(id), LifecycleState: autoscalingtypes.LifecycleState(lifecycle)}
}

type fakeControl struct {
	observations    api.OperatorCapacityObservationsResponse
	observationErr  error
	instances       api.OperatorWorkerInstancesResponse
	drainedID       string
	drainRequest    *api.OperatorDrainWorkerInstanceRequest
	instanceQueries [][]string
}

func (f *fakeControl) CapacityObservations(context.Context) (api.OperatorCapacityObservationsResponse, error) {
	return f.observations, f.observationErr
}

func (f *fakeControl) WorkerInstances(_ context.Context, _ string, resourceIDs, _ []string, _ int32) (api.OperatorWorkerInstancesResponse, error) {
	f.instanceQueries = append(f.instanceQueries, append([]string(nil), resourceIDs...))
	return f.instances, nil
}

func (f *fakeControl) DrainWorkerInstance(_ context.Context, id string, request api.OperatorDrainWorkerInstanceRequest) (api.OperatorWorkerInstance, error) {
	f.drainedID = id
	f.drainRequest = &request
	return api.OperatorWorkerInstance{ID: id, State: "draining", ClaimVersion: request.ExpectedClaimVersion + 1}, nil
}

type fakeAutoScaling struct {
	group         autoscalingtypes.AutoScalingGroup
	describeCalls int
	setDesired    *autoscaling.SetDesiredCapacityInput
	terminate     *autoscaling.TerminateInstanceInAutoScalingGroupInput
	complete      *autoscaling.CompleteLifecycleActionInput
}

func (f *fakeAutoScaling) DescribeAutoScalingGroups(context.Context, *autoscaling.DescribeAutoScalingGroupsInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	f.describeCalls++
	return &autoscaling.DescribeAutoScalingGroupsOutput{AutoScalingGroups: []autoscalingtypes.AutoScalingGroup{f.group}}, nil
}

func (f *fakeAutoScaling) SetDesiredCapacity(_ context.Context, input *autoscaling.SetDesiredCapacityInput, _ ...func(*autoscaling.Options)) (*autoscaling.SetDesiredCapacityOutput, error) {
	f.setDesired = input
	return &autoscaling.SetDesiredCapacityOutput{}, nil
}

func (f *fakeAutoScaling) TerminateInstanceInAutoScalingGroup(_ context.Context, input *autoscaling.TerminateInstanceInAutoScalingGroupInput, _ ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error) {
	f.terminate = input
	return &autoscaling.TerminateInstanceInAutoScalingGroupOutput{}, nil
}

func (f *fakeAutoScaling) CompleteLifecycleAction(_ context.Context, input *autoscaling.CompleteLifecycleActionInput, _ ...func(*autoscaling.Options)) (*autoscaling.CompleteLifecycleActionOutput, error) {
	f.complete = input
	return &autoscaling.CompleteLifecycleActionOutput{}, nil
}

func ptr[T any](value T) *T { return &value }
