package dbtest

import "github.com/google/uuid"

var DefaultOrgID = uuid.MustParse("00000000-0000-0000-0000-000000000000")

const (
	DefaultRegionID       = "us-east-1"
	DefaultProvider       = "aws"
	DefaultProviderRegion = "us-east-1"
	DefaultRegionDisplay  = "US East (N. Virginia)"
	DefaultWorkerGroupID  = "us-east-1-worker-group-1"
)
