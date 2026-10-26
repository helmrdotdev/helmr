package workerapi

import (
	"time"

	"github.com/helmrdotdev/helmr/internal/bootseed"
)

type BootSeedExecutor struct {
	Network           *NetworkCapabilities `json:"network,omitempty"`
	Identity          AllocationIdentity   `json:"identity"`
	ChannelCredential []byte               `json:"channel_credential"`
}

type BootSeedAllocationDelivery struct {
	Identity          AllocationIdentity `json:"identity"`
	Shape             AllocationShape    `json:"shape"`
	ExpiresAt         time.Time          `json:"expires_at"`
	ChannelCredential []byte             `json:"channel_credential"`
}

type BootSeedPublication struct {
	Executor      BootSeedExecutor       `json:"executor"`
	Manifest      bootseed.Manifest      `json:"manifest"`
	Qualification bootseed.Qualification `json:"qualification"`
}

type BootSeedFailure struct {
	Executor BootSeedExecutor `json:"executor"`
	Code     string           `json:"code"`
}
