package dispatch

import "time"

const dispatchMeasurementEnabled = "HELMR_MEASURE_DISPATCH"

func percentileIndex(length, percentile int) int {
	return max(0, (length*percentile+99)/100-1)
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration.Microseconds()) / 1000
}
