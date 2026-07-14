package enrollment

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestPrepareAWSGroupProjectsCommonSpec(t *testing.T) {
	boundary, desired, err := PrepareAWSGroup(AWSGroupBoundary{
		Spec:   workergroup.Spec{ID: "group-1", AllowsRun: true},
		Region: "us-east-1", AccountID: "123456789012", AutoScalingGroup: "workers",
		InstanceProfileARN: "arn:aws:iam::123456789012:instance-profile/workers",
		LaunchAMIID:        "ami-1", AMIIDs: []string{"ami-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if boundary.Name != "group-1" || desired.Spec != boundary.Spec {
		t.Fatalf("boundary = %#v, desired = %#v", boundary, desired)
	}
	if len(desired.AllowedAttestationFingerprints) != 1 || desired.LaunchAttestationFingerprint != desired.AllowedAttestationFingerprints[0] {
		t.Fatalf("desired = %#v", desired)
	}
}
