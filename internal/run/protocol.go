package run

import "time"

const (
	ReservationTTL  = 5 * time.Minute
	StartDeadline   = time.Minute
	LeaseTTL        = 5 * time.Minute
	FinalizationTTL = 30 * time.Minute
)
