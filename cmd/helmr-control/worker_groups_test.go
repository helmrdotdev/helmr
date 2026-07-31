package main

import (
	"strings"
	"testing"
)

func TestValidateAWSRegionMapping(t *testing.T) {
	tests := []struct {
		name           string
		provider       string
		providerRegion string
		groups         []configuredWorkerGroup
		wantError      string
	}{
		{
			name:           "matching AWS mapping",
			provider:       "aws",
			providerRegion: "us-east-1",
			groups:         []configuredWorkerGroup{{ID: "run", Region: "us-east-1"}, {ID: "build", Region: "us-east-1"}},
		},
		{
			name:           "wrong provider",
			provider:       "local",
			providerRegion: "us-east-1",
			wantError:      "HELMR_PROVIDER must be aws",
		},
		{
			name:           "mismatched group region",
			provider:       "aws",
			providerRegion: "us-east-1",
			groups:         []configuredWorkerGroup{{ID: "run", Region: "us-west-2"}},
			wantError:      `worker group "run" region "us-west-2" must match HELMR_PROVIDER_REGION "us-east-1"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateAWSRegionMapping(test.provider, test.providerRegion, test.groups)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}
