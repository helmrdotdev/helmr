package controlplane

import (
	"uuid"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

const controlplaneTestWorkerGroup = "01900000-0000-7000-8000-000000000701"

var (
	controlplaneTestWorkerGroupID   = uuid.MustParse(controlplaneTestWorkerGroup)
	controlplaneTestWorkerGroupDBID = pgvalue.UUID(controlplaneTestWorkerGroupID)
	controlplaneOtherWorkerGroupID  = uuid.MustParse("01900000-0000-7000-8000-000000000702")
)
