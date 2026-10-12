package telemetry

import "errors"

var ErrHistoricalUnavailable = errors.New("telemetry historical store unavailable")

type LaggingError struct {
	WatermarkSeq int64
	WantSeq      int64
}

func (e LaggingError) Error() string {
	return "telemetry replay is lagging"
}

type RejectedRow struct {
	Index int
	Err   error
}
