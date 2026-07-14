package fleet

import (
	"context"
	"errors"
	"reflect"
	"testing"

	awsdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	awstypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
)

func TestAWSActuatorDescribesExactConfiguredGroup(t *testing.T) {
	client := &fakeAutoScaling{groups: map[string]awstypes.AutoScalingGroup{
		"helmr-run": {
			AutoScalingGroupName: awsdk.String("helmr-run"), DesiredCapacity: awsdk.Int32(2),
			Instances: []awstypes.Instance{{InstanceId: awsdk.String("i-run"), ProtectedFromScaleIn: awsdk.Bool(true), LifecycleState: awstypes.LifecycleStateInService}},
		},
	}}
	actuator := mustAWSActuator(t, client)
	state, err := actuator.Describe(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	want := ProviderState{Desired: 2, Instances: map[string]ProviderInstance{
		"i-run": {ID: "i-run", ProtectedFromScaleIn: true, Lifecycle: string(awstypes.LifecycleStateInService)},
	}}
	if !reflect.DeepEqual(state, want) {
		t.Fatalf("state = %#v, want %#v", state, want)
	}
	if !reflect.DeepEqual(client.described, []string{"helmr-run"}) {
		t.Fatalf("described groups = %v", client.described)
	}
}

func TestAWSActuatorUsesAbsoluteDesiredAndExactInstance(t *testing.T) {
	client := &fakeAutoScaling{groups: map[string]awstypes.AutoScalingGroup{
		"helmr-run": {
			AutoScalingGroupName: awsdk.String("helmr-run"), DesiredCapacity: awsdk.Int32(1),
			Instances: []awstypes.Instance{{InstanceId: awsdk.String("i-run"), ProtectedFromScaleIn: awsdk.Bool(true)}},
		},
	}}
	actuator := mustAWSActuator(t, client)
	ctx := context.Background()
	if err := actuator.SetDesired(ctx, "run", 3); err != nil {
		t.Fatal(err)
	}
	if err := actuator.SetScaleInProtection(ctx, "run", "i-run", false); err != nil {
		t.Fatal(err)
	}
	if err := actuator.Terminate(ctx, "run", "i-run", true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client.desired, []desiredAWSCall{{asg: "helmr-run", desired: 3, honorCooldown: false}}) {
		t.Fatalf("desired calls = %#v", client.desired)
	}
	if !reflect.DeepEqual(client.protection, []protectionAWSCall{{asg: "helmr-run", ids: []string{"i-run"}, protected: false}}) {
		t.Fatalf("protection calls = %#v", client.protection)
	}
	if !reflect.DeepEqual(client.terminated, []terminateAWSCall{{id: "i-run", decrement: true}}) {
		t.Fatalf("termination calls = %#v", client.terminated)
	}
}

func TestAWSActuatorFailsClosedForUnknownOrForeignInstance(t *testing.T) {
	client := &fakeAutoScaling{groups: map[string]awstypes.AutoScalingGroup{
		"helmr-run": {AutoScalingGroupName: awsdk.String("helmr-run"), Instances: []awstypes.Instance{{InstanceId: awsdk.String("i-run")}}},
	}}
	actuator := mustAWSActuator(t, client)
	ctx := context.Background()
	if _, err := actuator.Describe(ctx, "build"); err == nil {
		t.Fatal("unknown group describe succeeded")
	}
	if err := actuator.SetScaleInProtection(ctx, "run", "i-foreign", false); err == nil {
		t.Fatal("foreign instance unprotect succeeded")
	}
	if err := actuator.Terminate(ctx, "run", "i-foreign", true); err == nil {
		t.Fatal("foreign instance termination succeeded")
	}
	if len(client.protection) != 0 || len(client.terminated) != 0 {
		t.Fatalf("foreign instance reached mutation: protection=%#v termination=%#v", client.protection, client.terminated)
	}
}

func TestAWSActuatorRequiresDesiredDecrement(t *testing.T) {
	client := &fakeAutoScaling{groups: map[string]awstypes.AutoScalingGroup{
		"helmr-run": {AutoScalingGroupName: awsdk.String("helmr-run"), Instances: []awstypes.Instance{{InstanceId: awsdk.String("i-run")}}},
	}}
	actuator := mustAWSActuator(t, client)
	if err := actuator.Terminate(context.Background(), "run", "i-run", false); err == nil {
		t.Fatal("termination without desired decrement succeeded")
	}
}

func TestAWSActuatorPreservesAmbiguousProviderError(t *testing.T) {
	client := &fakeAutoScaling{groups: map[string]awstypes.AutoScalingGroup{
		"helmr-run": {AutoScalingGroupName: awsdk.String("helmr-run")},
	}, desiredErr: errors.New("response lost")}
	actuator := mustAWSActuator(t, client)
	if err := actuator.SetDesired(context.Background(), "run", 1); !errors.Is(err, client.desiredErr) {
		t.Fatalf("SetDesired error = %v, want wrapped provider error", err)
	}
}

func mustAWSActuator(t *testing.T, client AutoScalingAPI) *AWSActuator {
	t.Helper()
	actuator, err := NewAWSActuator(client, map[string]string{"run": "helmr-run"})
	if err != nil {
		t.Fatal(err)
	}
	return actuator
}

type desiredAWSCall struct {
	asg           string
	desired       int32
	honorCooldown bool
}

type protectionAWSCall struct {
	asg       string
	ids       []string
	protected bool
}

type terminateAWSCall struct {
	id        string
	decrement bool
}

type fakeAutoScaling struct {
	groups     map[string]awstypes.AutoScalingGroup
	described  []string
	desired    []desiredAWSCall
	protection []protectionAWSCall
	terminated []terminateAWSCall
	desiredErr error
}

func (client *fakeAutoScaling) DescribeAutoScalingGroups(_ context.Context, input *autoscaling.DescribeAutoScalingGroupsInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	client.described = append(client.described, input.AutoScalingGroupNames...)
	output := &autoscaling.DescribeAutoScalingGroupsOutput{}
	for _, name := range input.AutoScalingGroupNames {
		if group, exists := client.groups[name]; exists {
			output.AutoScalingGroups = append(output.AutoScalingGroups, group)
		}
	}
	return output, nil
}

func (client *fakeAutoScaling) SetDesiredCapacity(_ context.Context, input *autoscaling.SetDesiredCapacityInput, _ ...func(*autoscaling.Options)) (*autoscaling.SetDesiredCapacityOutput, error) {
	client.desired = append(client.desired, desiredAWSCall{
		asg: awsdk.ToString(input.AutoScalingGroupName), desired: awsdk.ToInt32(input.DesiredCapacity), honorCooldown: awsdk.ToBool(input.HonorCooldown),
	})
	return &autoscaling.SetDesiredCapacityOutput{}, client.desiredErr
}

func (client *fakeAutoScaling) SetInstanceProtection(_ context.Context, input *autoscaling.SetInstanceProtectionInput, _ ...func(*autoscaling.Options)) (*autoscaling.SetInstanceProtectionOutput, error) {
	client.protection = append(client.protection, protectionAWSCall{
		asg: awsdk.ToString(input.AutoScalingGroupName), ids: append([]string(nil), input.InstanceIds...), protected: awsdk.ToBool(input.ProtectedFromScaleIn),
	})
	return &autoscaling.SetInstanceProtectionOutput{}, nil
}

func (client *fakeAutoScaling) TerminateInstanceInAutoScalingGroup(_ context.Context, input *autoscaling.TerminateInstanceInAutoScalingGroupInput, _ ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error) {
	client.terminated = append(client.terminated, terminateAWSCall{
		id: awsdk.ToString(input.InstanceId), decrement: awsdk.ToBool(input.ShouldDecrementDesiredCapacity),
	})
	return &autoscaling.TerminateInstanceInAutoScalingGroupOutput{}, nil
}
