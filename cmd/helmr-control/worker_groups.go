package main

import (
	"fmt"
	"strings"

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
	ObservationTTL     int32                `json:"observation_ttl_seconds"`
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

func validateAWSRegionMapping(provider, providerRegion string, groups []configuredWorkerGroup) error {
	if strings.TrimSpace(provider) != "aws" {
		return fmt.Errorf("HELMR_PROVIDER must be aws for the AWS control entry")
	}
	providerRegion = strings.TrimSpace(providerRegion)
	for _, group := range groups {
		if strings.TrimSpace(group.Region) != providerRegion {
			return fmt.Errorf(
				"worker group %q region %q must match HELMR_PROVIDER_REGION %q",
				group.ID,
				group.Region,
				providerRegion,
			)
		}
	}
	return nil
}
