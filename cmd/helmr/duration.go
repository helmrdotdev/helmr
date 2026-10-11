package main

import (
	"github.com/helmrdotdev/helmr/internal/api"
	"math"
	"time"
)

func parseDuration(value string) (time.Duration, error) {
	milliseconds, err := api.ParseDurationMilliseconds(value, "duration", 1, math.MaxInt64/int64(time.Millisecond))
	if err != nil {
		return 0, err
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}
