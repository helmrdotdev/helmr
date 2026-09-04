package dbtest

import (
	"uuid"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

var (
	DefaultOrgID           = uuid.MustParse("00000000-0000-0000-0000-000000000000")
	DefaultWorkerGroupUUID = uuid.MustParse("01900000-0000-7000-8000-000000000002")
	DefaultWorkerGroupID   = pgvalue.UUID(DefaultWorkerGroupUUID)
)

const (
	DefaultRegionID        = "us-east-1"
	DefaultRegionDisplay   = "US East (N. Virginia)"
	DefaultWorkerGroupName = "run-workers"
	DefaultWorkerPoolID    = "01900000-0000-7000-8000-000000000001"
	DefaultRuntimeID       = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	DefaultCPUConfigID     = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)
