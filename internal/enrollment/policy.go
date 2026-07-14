package enrollment

import (
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func PrepareAWSGroup(group AWSGroupBoundary) (AWSGroupBoundary, workergroup.Desired, error) {
	group, err := NormalizeAWSGroupBoundary(group)
	if err != nil {
		return AWSGroupBoundary{}, workergroup.Desired{}, err
	}
	policyFingerprint, err := AWSPolicyFingerprint(group)
	if err != nil {
		return AWSGroupBoundary{}, workergroup.Desired{}, err
	}
	allowed := make([]string, 0, len(group.AMIIDs))
	for _, amiID := range group.AMIIDs {
		fingerprint, err := AWSAttestationFingerprint(group, amiID)
		if err != nil {
			return AWSGroupBoundary{}, workergroup.Desired{}, err
		}
		allowed = append(allowed, fingerprint)
	}
	launch, err := AWSAttestationFingerprint(group, group.LaunchAMIID)
	if err != nil {
		return AWSGroupBoundary{}, workergroup.Desired{}, err
	}
	return group, workergroup.Desired{
		Spec: group.Spec, Capacity: group.Capacity, EnrollmentPolicyFingerprint: policyFingerprint,
		AllowedAttestationFingerprints: allowed, LaunchAttestationFingerprint: launch,
	}, nil
}
