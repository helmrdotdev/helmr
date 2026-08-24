package dbtest

import "github.com/google/uuid"

var DefaultOrgID = uuid.MustParse("00000000-0000-0000-0000-000000000000")

const (
	DefaultRegionID      = "us-east-1"
	DefaultRegionDisplay = "US East (N. Virginia)"
	DefaultWorkerGroupID = "us-east-1-worker-group-1"
	DefaultWorkerPoolID  = "01900000-0000-7000-8000-000000000001"
	DefaultRuntimeID     = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	DefaultCPUConfigID   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)
