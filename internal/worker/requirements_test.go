package worker

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
)

func TestValidateLeaseRequirementsAcceptsDefaultNetwork(t *testing.T) {
	if err := validateLeaseRequirements(testCapabilities(), testLease(), testRun(testRequirements())); err != nil {
		t.Fatal(err)
	}
}

func TestValidateLeaseRequirementsRejectsUnsupportedNetworkPolicy(t *testing.T) {
	tests := map[string]struct {
		mutateCapabilities func(*api.WorkerCapabilities)
		mutateNetwork      func(*compute.NetworkPolicy)
		want               string
	}{
		"block internet": {
			mutateCapabilities: func(capabilities *api.WorkerCapabilities) {
				capabilities.Network.BlockInternet = false
			},
			mutateNetwork: func(network *compute.NetworkPolicy) {
				network.Internet = false
			},
			want: "cannot block internet",
		},
		"deny cidr": {
			mutateCapabilities: func(capabilities *api.WorkerCapabilities) {
				capabilities.Network.DenyCIDRs = false
			},
			mutateNetwork: func(network *compute.NetworkPolicy) {
				network.Deny = []string{"10.0.0.0/8"}
			},
			want: "cannot enforce CIDR deny rules",
		},
		"allow cidr": {
			mutateNetwork: func(network *compute.NetworkPolicy) {
				network.Allow = []string{"203.0.113.0/24"}
			},
			want: "cannot enforce network allow rules",
		},
		"allow domain": {
			mutateCapabilities: func(capabilities *api.WorkerCapabilities) {
				capabilities.Network.AllowCIDRs = true
			},
			mutateNetwork: func(network *compute.NetworkPolicy) {
				network.Allow = []string{"api.github.com"}
			},
			want: "cannot enforce domain allow rule",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			capabilities := testCapabilities()
			if tt.mutateCapabilities != nil {
				tt.mutateCapabilities(&capabilities)
			}
			requirements := testRequirements()
			tt.mutateNetwork(&requirements.Network)

			err := validateLeaseRequirements(capabilities, testLease(), testRun(requirements))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func testLease() api.WorkerRunLease {
	return api.WorkerRunLease{
		ID:              "lease-1",
		RunID:           "run-1",
		ProtocolVersion: api.CurrentWorkerProtocolVersion,
	}
}

func testRun(requirements compute.RunRuntimeRequirements) api.WorkerRun {
	return api.WorkerRun{
		ID:                    "run-1",
		TaskID:                "deploy",
		WorkerProtocolVersion: api.CurrentWorkerProtocolVersion,
		Requirements:          requirements,
	}
}
