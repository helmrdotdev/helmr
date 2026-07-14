package main

import (
	"github.com/helmrdotdev/helmr/internal/enrollment"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

type configuredWorkerGroup struct {
	ID                 string               `json:"id"`
	Name               string               `json:"name"`
	Description        string               `json:"description,omitempty"`
	Region             string               `json:"region"`
	AccountID          string               `json:"account_id"`
	AutoScalingGroup   string               `json:"autoscaling_group"`
	InstanceProfileARN string               `json:"instance_profile_arn"`
	LaunchAMIID        string               `json:"launch_ami_id"`
	AMIIDs             []string             `json:"ami_ids"`
	AllowsRun          bool                 `json:"allows_run"`
	AllowsBuild        bool                 `json:"allows_build"`
	InstanceCapacity   workergroup.Capacity `json:"instance_capacity"`
}

func (group configuredWorkerGroup) awsWorkerGroup() enrollment.AWSGroupBoundary {
	return enrollment.AWSGroupBoundary{
		Spec: workergroup.Spec{
			ID: group.ID, Name: group.Name, Description: group.Description,
			AllowsRun: group.AllowsRun, AllowsBuild: group.AllowsBuild,
		},
		Capacity: group.InstanceCapacity,
		Region:   group.Region, AccountID: group.AccountID,
		AutoScalingGroup: group.AutoScalingGroup, InstanceProfileARN: group.InstanceProfileARN,
		LaunchAMIID: group.LaunchAMIID, AMIIDs: group.AMIIDs,
	}
}
