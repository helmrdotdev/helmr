package awscapacity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	awsdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/helmrdotdev/helmr/internal/api"
)

var ec2InstanceIDPattern = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)

type Control interface {
	CapacityObservations(context.Context) (api.OperatorCapacityObservationsResponse, error)
	WorkerInstances(context.Context, string, []string, []string, int32) (api.OperatorWorkerInstancesResponse, error)
	DrainWorkerInstance(context.Context, string, api.OperatorDrainWorkerInstanceRequest) (api.OperatorWorkerInstance, error)
}

type AutoScaling interface {
	DescribeAutoScalingGroups(context.Context, *autoscaling.DescribeAutoScalingGroupsInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
	SetDesiredCapacity(context.Context, *autoscaling.SetDesiredCapacityInput, ...func(*autoscaling.Options)) (*autoscaling.SetDesiredCapacityOutput, error)
	TerminateInstanceInAutoScalingGroup(context.Context, *autoscaling.TerminateInstanceInAutoScalingGroupInput, ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error)
	CompleteLifecycleAction(context.Context, *autoscaling.CompleteLifecycleActionInput, ...func(*autoscaling.Options)) (*autoscaling.CompleteLifecycleActionOutput, error)
}

type Reconciler struct {
	log     *slog.Logger
	control Control
	aws     AutoScaling
	config  Config
	now     func() time.Time
}

func NewReconciler(log *slog.Logger, control Control, aws AutoScaling, config Config) (*Reconciler, error) {
	if control == nil || aws == nil {
		return nil, errors.New("capacity reconciler requires Control and Auto Scaling clients")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{log: log, control: control, aws: aws, config: config, now: time.Now}, nil
}

func (r *Reconciler) Reconcile(ctx context.Context) error {
	observations, err := r.control.CapacityObservations(ctx)
	if err != nil {
		return fmt.Errorf("read Control capacity observations: %w", err)
	}
	byGroup := make(map[string]api.OperatorCapacityObservation, len(observations.Observations))
	for _, observation := range observations.Observations {
		if _, duplicate := byGroup[observation.WorkerGroupID]; duplicate {
			return fmt.Errorf("Control returned duplicate capacity observation for Worker group %q", observation.WorkerGroupID)
		}
		byGroup[observation.WorkerGroupID] = observation
	}
	for _, group := range r.config.Groups {
		// A missing observation blocks new scale decisions inside reconcileGroup,
		// but it must not invalidate an exact termination_ready receipt that
		// Control has already issued for this group.
		observation := byGroup[group.WorkerGroupID]
		if err := r.reconcileGroup(ctx, group, observation); err != nil {
			return fmt.Errorf("reconcile Worker group %q: %w", group.WorkerGroupID, err)
		}
	}
	return nil
}

func (r *Reconciler) reconcileGroup(ctx context.Context, config GroupConfig, observation api.OperatorCapacityObservation) error {
	group, err := r.describeGroup(ctx, config.AutoScalingGroupName)
	if err != nil {
		return err
	}
	minSize, maxSize, err := providerCapacityBounds(config, group)
	if err != nil {
		return err
	}
	providerInstances := map[string]autoscalingtypes.Instance{}
	for _, instance := range group.Instances {
		id := awsdk.ToString(instance.InstanceId)
		if id == "" {
			return errors.New("Auto Scaling group returned an instance without an ID")
		}
		providerInstances[id] = instance
	}
	instances, err := r.logicalInstances(ctx, config.WorkerGroupID, providerInstances)
	if err != nil {
		return err
	}

	for _, instance := range instances.WorkerInstances {
		if instance.State != "termination_ready" {
			continue
		}
		if err := validateProviderLocator(instance.ResourceID); err != nil {
			return fmt.Errorf("terminal Worker %q: %w", instance.ID, err)
		}
		provider, present := providerInstances[instance.ResourceID]
		if !present {
			continue
		}
		lifecycle := string(provider.LifecycleState)
		if strings.Contains(lifecycle, "Terminating:Wait") {
			_, err := r.aws.CompleteLifecycleAction(ctx, &autoscaling.CompleteLifecycleActionInput{
				AutoScalingGroupName:  awsdk.String(config.AutoScalingGroupName),
				LifecycleHookName:     awsdk.String(config.TerminationLifecycleHookName),
				InstanceId:            awsdk.String(instance.ResourceID),
				LifecycleActionResult: awsdk.String("CONTINUE"),
			})
			if err != nil {
				return fmt.Errorf("complete termination lifecycle action for %q: %w", instance.ResourceID, err)
			}
			return nil
		}
		if strings.HasPrefix(lifecycle, "Terminating") {
			return nil
		}
		_, err := r.aws.TerminateInstanceInAutoScalingGroup(ctx, &autoscaling.TerminateInstanceInAutoScalingGroupInput{
			InstanceId:                     awsdk.String(instance.ResourceID),
			ShouldDecrementDesiredCapacity: awsdk.Bool(true),
		})
		if err != nil {
			return fmt.Errorf("terminate exact Worker host %q: %w", instance.ResourceID, err)
		}
		return nil
	}

	for _, instance := range instances.WorkerInstances {
		if instance.State == "draining" {
			return nil
		}
	}
	if maxSize == 0 {
		return nil
	}
	if observation.ObservedAt.IsZero() || r.now().Sub(observation.ObservedAt) > r.config.ObservationMaxAge || observation.ObservedAt.After(r.now().Add(time.Minute)) {
		return errors.New("capacity observation is stale or invalid")
	}
	if observation.GroupState != "active" {
		return nil
	}
	desired := awsdk.ToInt32(group.DesiredCapacity)
	missing := missingWorkers(config, observation, desired, maxSize)
	if missing > 0 && desired < maxSize {
		target := min(maxSize, desired+missing)
		_, err := r.aws.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
			AutoScalingGroupName: awsdk.String(config.AutoScalingGroupName),
			DesiredCapacity:      awsdk.Int32(target),
			HonorCooldown:        awsdk.Bool(true),
		})
		if err != nil {
			return fmt.Errorf("set desired capacity to %d: %w", target, err)
		}
		return nil
	}

	ready := readyWorkers(config, observation)
	if !hasQueuedDemand(config, observation) && desired > minSize && int64(desired) == ready && ready > int64(minSize) {
		candidates := make([]api.OperatorWorkerInstance, 0)
		for _, instance := range instances.WorkerInstances {
			provider, present := providerInstances[instance.ResourceID]
			if instance.State != "active" || instance.CurrentEpoch == nil || !present || string(provider.LifecycleState) != "InService" {
				continue
			}
			if err := validateProviderLocator(instance.ResourceID); err != nil {
				return fmt.Errorf("active Worker %q: %w", instance.ID, err)
			}
			candidates = append(candidates, instance)
		}
		sort.Slice(candidates, func(left, right int) bool {
			if candidates[left].CreatedAt.Equal(candidates[right].CreatedAt) {
				return candidates[left].ID < candidates[right].ID
			}
			return candidates[left].CreatedAt.Before(candidates[right].CreatedAt)
		})
		if len(candidates) > 0 {
			candidate := candidates[0]
			_, err := r.control.DrainWorkerInstance(ctx, candidate.ID, api.OperatorDrainWorkerInstanceRequest{
				ExpectedEpoch: *candidate.CurrentEpoch, ExpectedClaimVersion: candidate.ClaimVersion,
			})
			if err != nil {
				return fmt.Errorf("drain exact Worker %q: %w", candidate.ID, err)
			}
		}
	}
	return nil
}

func (r *Reconciler) logicalInstances(
	ctx context.Context,
	workerGroupID string,
	providerInstances map[string]autoscalingtypes.Instance,
) (api.OperatorWorkerInstancesResponse, error) {
	// Keep the encoded request line comfortably below common proxy and load
	// balancer limits even when every EC2 instance ID has its maximum length.
	const batchSize = 200
	resourceIDs := make([]string, 0, len(providerInstances))
	for resourceID := range providerInstances {
		resourceIDs = append(resourceIDs, resourceID)
	}
	sort.Strings(resourceIDs)
	result := api.OperatorWorkerInstancesResponse{WorkerInstances: []api.OperatorWorkerInstance{}}
	seen := make(map[string]struct{}, len(resourceIDs))
	for offset := 0; offset < len(resourceIDs); offset += batchSize {
		end := min(offset+batchSize, len(resourceIDs))
		response, err := r.control.WorkerInstances(
			ctx,
			workerGroupID,
			resourceIDs[offset:end],
			[]string{"active", "draining", "termination_ready"},
			batchSize,
		)
		if err != nil {
			return result, fmt.Errorf("list logical Worker instances: %w", err)
		}
		for _, instance := range response.WorkerInstances {
			if instance.WorkerGroupID != workerGroupID {
				return result, fmt.Errorf("Control returned Worker %q from unexpected group %q", instance.ID, instance.WorkerGroupID)
			}
			if _, present := providerInstances[instance.ResourceID]; !present {
				return result, fmt.Errorf("Control returned Worker %q outside the requested provider inventory", instance.ID)
			}
			if _, duplicate := seen[instance.ResourceID]; duplicate {
				return result, fmt.Errorf("Control returned duplicate current Workers for provider locator %q", instance.ResourceID)
			}
			seen[instance.ResourceID] = struct{}{}
			result.WorkerInstances = append(result.WorkerInstances, instance)
		}
	}
	return result, nil
}

func (r *Reconciler) describeGroup(ctx context.Context, name string) (autoscalingtypes.AutoScalingGroup, error) {
	output, err := r.aws.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{name},
	})
	if err != nil {
		return autoscalingtypes.AutoScalingGroup{}, fmt.Errorf("describe Auto Scaling group %q: %w", name, err)
	}
	if output == nil || len(output.AutoScalingGroups) != 1 || awsdk.ToString(output.AutoScalingGroups[0].AutoScalingGroupName) != name {
		return autoscalingtypes.AutoScalingGroup{}, fmt.Errorf("describe Auto Scaling group %q did not return one exact match", name)
	}
	return output.AutoScalingGroups[0], nil
}

func missingWorkers(config GroupConfig, observation api.OperatorCapacityObservation, desired int32, maxSize int32) int32 {
	if maxSize <= 0 {
		return 0
	}
	runQueued := config.AllowsRun && observation.Run != nil && observation.Run.QueuedItems > 0
	buildQueued := config.AllowsBuild && observation.Build != nil && observation.Build.QueuedItems > 0
	if !runQueued && !buildQueued {
		return 0
	}
	ready := demandedReadyWorkers(observation, runQueued, buildQueued)
	pending := max(int64(desired)-ready, 0)
	pendingCapacity := multiplyCapacity(config.InstanceCapacity, pending)

	queuedShared := api.OperatorResourceVector{}
	availableShared := api.OperatorResourceVector{}
	if runQueued {
		queuedShared = addCapacity(queuedShared, sharedCapacity(observation.Run.QueuedResources))
		availableShared = sharedCapacity(observation.Run.AvailableCapacity)
	}
	if buildQueued {
		queuedShared = addCapacity(queuedShared, sharedCapacity(observation.Build.QueuedResources))
		if runQueued {
			availableShared = minSharedCapacity(availableShared, observation.Build.AvailableCapacity)
		} else {
			availableShared = sharedCapacity(observation.Build.AvailableCapacity)
		}
	}
	availableShared = addCapacity(availableShared, sharedCapacity(pendingCapacity))
	missing := max(
		workersForShortage(queuedShared.CPUMillis, availableShared.CPUMillis, config.InstanceCapacity.CPUMillis),
		workersForShortage(queuedShared.MemoryBytes, availableShared.MemoryBytes, config.InstanceCapacity.MemoryBytes),
		workersForShortage(queuedShared.GuestEphemeralDiskBytes, availableShared.GuestEphemeralDiskBytes, config.InstanceCapacity.GuestEphemeralDiskBytes),
	)
	if runQueued {
		available := addCapacity(observation.Run.AvailableCapacity, pendingCapacity)
		missing = max(missing,
			workersForShortage(observation.Run.QueuedResources.VMSlots, available.VMSlots, config.InstanceCapacity.VMSlots),
			workersForShortage(observation.Run.QueuedResources.RunConsumers, available.RunConsumers, config.InstanceCapacity.RunConsumers),
		)
	}
	if buildQueued {
		available := addCapacity(observation.Build.AvailableCapacity, pendingCapacity)
		missing = max(missing, workersForShortage(
			observation.Build.QueuedResources.BuildExecutors,
			available.BuildExecutors,
			config.InstanceCapacity.BuildExecutors,
		))
	}
	return int32(min(missing, int64(maxSize)))
}

func providerCapacityBounds(config GroupConfig, group autoscalingtypes.AutoScalingGroup) (int32, int32, error) {
	minSize := awsdk.ToInt32(group.MinSize)
	maxSize := awsdk.ToInt32(group.MaxSize)
	desired := awsdk.ToInt32(group.DesiredCapacity)
	if minSize < 0 || maxSize < 0 || minSize > maxSize || desired < minSize || desired > maxSize {
		return 0, 0, errors.New("Auto Scaling group returned invalid min, max, or desired capacity")
	}
	if maxSize == 0 {
		return minSize, maxSize, nil
	}
	for name, value := range map[string]int64{
		"cpu_millis":                 config.InstanceCapacity.CPUMillis,
		"memory_bytes":               config.InstanceCapacity.MemoryBytes,
		"guest_ephemeral_disk_bytes": config.InstanceCapacity.GuestEphemeralDiskBytes,
		"vm_slots":                   config.InstanceCapacity.VMSlots,
		"run_consumers":              config.InstanceCapacity.RunConsumers,
		"build_executors":            config.InstanceCapacity.BuildExecutors,
	} {
		if value > 0 && value > math.MaxInt64/int64(maxSize) {
			return 0, 0, fmt.Errorf("Auto Scaling group max size overflows %s capacity", name)
		}
	}
	return minSize, maxSize, nil
}

func workersForShortage(required, available, perWorker int64) int64 {
	if required <= available || perWorker <= 0 {
		return 0
	}
	shortage := required - available
	return (shortage + perWorker - 1) / perWorker
}

func demandedReadyWorkers(observation api.OperatorCapacityObservation, runQueued, buildQueued bool) int64 {
	ready := int64(-1)
	if runQueued {
		ready = observation.Run.ReadyWorkers
	}
	if buildQueued && (ready < 0 || observation.Build.ReadyWorkers < ready) {
		ready = observation.Build.ReadyWorkers
	}
	return max(ready, 0)
}

func sharedCapacity(capacity api.OperatorResourceVector) api.OperatorResourceVector {
	return api.OperatorResourceVector{
		CPUMillis: capacity.CPUMillis, MemoryBytes: capacity.MemoryBytes,
		GuestEphemeralDiskBytes: capacity.GuestEphemeralDiskBytes,
	}
}

func minSharedCapacity(left, right api.OperatorResourceVector) api.OperatorResourceVector {
	return api.OperatorResourceVector{
		CPUMillis:               min(left.CPUMillis, right.CPUMillis),
		MemoryBytes:             min(left.MemoryBytes, right.MemoryBytes),
		GuestEphemeralDiskBytes: min(left.GuestEphemeralDiskBytes, right.GuestEphemeralDiskBytes),
	}
}

func readyWorkers(config GroupConfig, observation api.OperatorCapacityObservation) int64 {
	ready := int64(-1)
	if config.AllowsRun {
		if observation.Run == nil {
			return 0
		}
		ready = observation.Run.ReadyWorkers
	}
	if config.AllowsBuild {
		if observation.Build == nil {
			return 0
		}
		if ready < 0 || observation.Build.ReadyWorkers < ready {
			ready = observation.Build.ReadyWorkers
		}
	}
	return max(ready, 0)
}

func hasQueuedDemand(config GroupConfig, observation api.OperatorCapacityObservation) bool {
	return (config.AllowsRun && observation.Run != nil && observation.Run.QueuedItems > 0) ||
		(config.AllowsBuild && observation.Build != nil && observation.Build.QueuedItems > 0)
}

func multiplyCapacity(capacity api.OperatorResourceVector, count int64) api.OperatorResourceVector {
	return api.OperatorResourceVector{
		CPUMillis: capacity.CPUMillis * count, MemoryBytes: capacity.MemoryBytes * count,
		GuestEphemeralDiskBytes: capacity.GuestEphemeralDiskBytes * count,
		VMSlots:                 capacity.VMSlots * count, RunConsumers: capacity.RunConsumers * count,
		BuildExecutors: capacity.BuildExecutors * count,
	}
}

func addCapacity(left, right api.OperatorResourceVector) api.OperatorResourceVector {
	return api.OperatorResourceVector{
		CPUMillis:               left.CPUMillis + right.CPUMillis,
		MemoryBytes:             left.MemoryBytes + right.MemoryBytes,
		GuestEphemeralDiskBytes: left.GuestEphemeralDiskBytes + right.GuestEphemeralDiskBytes,
		VMSlots:                 left.VMSlots + right.VMSlots,
		RunConsumers:            left.RunConsumers + right.RunConsumers,
		BuildExecutors:          left.BuildExecutors + right.BuildExecutors,
	}
}

func validateProviderLocator(locator string) error {
	if !ec2InstanceIDPattern.MatchString(locator) {
		return fmt.Errorf("opaque deployment locator %q is not an EC2 instance ID for this AWS adapter", locator)
	}
	return nil
}
