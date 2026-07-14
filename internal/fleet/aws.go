package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"

	awsdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
)

type AutoScalingAPI interface {
	DescribeAutoScalingGroups(context.Context, *autoscaling.DescribeAutoScalingGroupsInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
	SetDesiredCapacity(context.Context, *autoscaling.SetDesiredCapacityInput, ...func(*autoscaling.Options)) (*autoscaling.SetDesiredCapacityOutput, error)
	SetInstanceProtection(context.Context, *autoscaling.SetInstanceProtectionInput, ...func(*autoscaling.Options)) (*autoscaling.SetInstanceProtectionOutput, error)
	TerminateInstanceInAutoScalingGroup(context.Context, *autoscaling.TerminateInstanceInAutoScalingGroupInput, ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error)
}

// AWSActuator maps provider-neutral group IDs to one immutable Auto Scaling
// group each. Unknown groups fail closed.
type AWSActuator struct {
	client AutoScalingAPI
	groups map[string]string
}

func NewAWSActuator(client AutoScalingAPI, groups map[string]string) (*AWSActuator, error) {
	if client == nil || len(groups) == 0 {
		return nil, errors.New("aws actuator requires a client and at least one group")
	}
	configured := make(map[string]string, len(groups))
	seenASG := make(map[string]string, len(groups))
	for groupID, asg := range groups {
		groupID = strings.TrimSpace(groupID)
		asg = strings.TrimSpace(asg)
		if groupID == "" || asg == "" {
			return nil, errors.New("aws actuator group IDs and auto scaling group names must not be empty")
		}
		if prior, exists := seenASG[asg]; exists {
			return nil, fmt.Errorf("auto scaling group %q is assigned to both %q and %q", asg, prior, groupID)
		}
		if _, exists := configured[groupID]; exists {
			return nil, fmt.Errorf("aws actuator group %q is duplicated", groupID)
		}
		configured[groupID] = asg
		seenASG[asg] = groupID
	}
	return &AWSActuator{client: client, groups: configured}, nil
}

func (a *AWSActuator) Describe(ctx context.Context, groupID string) (ProviderState, error) {
	asg, err := a.group(groupID)
	if err != nil {
		return ProviderState{}, err
	}
	output, err := a.client.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asg},
	})
	if err != nil {
		return ProviderState{}, fmt.Errorf("describe Auto Scaling group %q: %w", asg, err)
	}
	if output == nil {
		return ProviderState{}, fmt.Errorf("describe Auto Scaling group %q returned nil output", asg)
	}
	if len(output.AutoScalingGroups) != 1 || awsdk.ToString(output.AutoScalingGroups[0].AutoScalingGroupName) != asg {
		return ProviderState{}, fmt.Errorf("describe Auto Scaling group %q returned %d exact matches", asg, len(output.AutoScalingGroups))
	}
	group := output.AutoScalingGroups[0]
	state := ProviderState{
		Desired:   int(awsdk.ToInt32(group.DesiredCapacity)),
		Instances: make(map[string]ProviderInstance, len(group.Instances)),
	}
	for _, instance := range group.Instances {
		id := awsdk.ToString(instance.InstanceId)
		if id == "" {
			return ProviderState{}, fmt.Errorf("auto scaling group %q returned an instance without an ID", asg)
		}
		if _, duplicate := state.Instances[id]; duplicate {
			return ProviderState{}, fmt.Errorf("auto scaling group %q returned duplicate instance %q", asg, id)
		}
		state.Instances[id] = ProviderInstance{
			ID:                   id,
			ProtectedFromScaleIn: awsdk.ToBool(instance.ProtectedFromScaleIn),
			Lifecycle:            string(instance.LifecycleState),
		}
	}
	return state, nil
}

func (a *AWSActuator) SetDesired(ctx context.Context, groupID string, desired int) error {
	asg, err := a.group(groupID)
	if err != nil {
		return err
	}
	if desired < 0 || desired > int(^uint32(0)>>1) {
		return fmt.Errorf("invalid desired capacity %d", desired)
	}
	_, err = a.client.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
		AutoScalingGroupName: awsdk.String(asg),
		DesiredCapacity:      awsdk.Int32(int32(desired)),
		HonorCooldown:        awsdk.Bool(false),
	})
	if err != nil {
		return fmt.Errorf("set desired capacity for Auto Scaling group %q to %d: %w", asg, desired, err)
	}
	return nil
}

func (a *AWSActuator) SetScaleInProtection(ctx context.Context, groupID, instanceID string, protected bool) error {
	asg, err := a.group(groupID)
	if err != nil {
		return err
	}
	if instanceID = strings.TrimSpace(instanceID); instanceID == "" {
		return errors.New("instance ID is required")
	}
	if err := a.requireMember(ctx, groupID, instanceID); err != nil {
		return err
	}
	_, err = a.client.SetInstanceProtection(ctx, &autoscaling.SetInstanceProtectionInput{
		AutoScalingGroupName: awsdk.String(asg),
		InstanceIds:          []string{instanceID},
		ProtectedFromScaleIn: awsdk.Bool(protected),
	})
	if err != nil {
		return fmt.Errorf("set scale-in protection=%t for instance %q in Auto Scaling group %q: %w", protected, instanceID, asg, err)
	}
	return nil
}

func (a *AWSActuator) Terminate(ctx context.Context, groupID, instanceID string, decrementDesired bool) error {
	if _, err := a.group(groupID); err != nil {
		return err
	}
	if instanceID = strings.TrimSpace(instanceID); instanceID == "" {
		return errors.New("instance ID is required")
	}
	if !decrementDesired {
		return errors.New("aws fleet termination must decrement desired capacity")
	}
	if err := a.requireMember(ctx, groupID, instanceID); err != nil {
		return err
	}
	_, err := a.client.TerminateInstanceInAutoScalingGroup(ctx, &autoscaling.TerminateInstanceInAutoScalingGroupInput{
		InstanceId:                     awsdk.String(instanceID),
		ShouldDecrementDesiredCapacity: awsdk.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("terminate Auto Scaling instance %q with desired decrement: %w", instanceID, err)
	}
	return nil
}

func (a *AWSActuator) requireMember(ctx context.Context, groupID, instanceID string) error {
	state, err := a.Describe(ctx, groupID)
	if err != nil {
		return err
	}
	if _, exists := state.Instances[instanceID]; !exists {
		return fmt.Errorf("instance %q is not a member of configured group %q", instanceID, groupID)
	}
	return nil
}

func (a *AWSActuator) group(groupID string) (string, error) {
	if a == nil || a.client == nil {
		return "", errors.New("aws actuator is not configured")
	}
	asg, exists := a.groups[groupID]
	if !exists {
		return "", fmt.Errorf("unknown AWS fleet group %q", groupID)
	}
	return asg, nil
}
