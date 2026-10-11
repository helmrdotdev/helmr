package api

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
)

var durationPattern = regexp.MustCompile(`^([1-9][0-9]*)(ms|s|m|h|d)$`)

func ParseDurationMilliseconds(raw string, label string, minValue int64, maxValue int64) (int64, error) {
	match := durationPattern.FindStringSubmatch(raw)
	if match == nil {
		return 0, fmt.Errorf("%s must match %s", label, durationPattern.String())
	}
	value, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s is outside the supported range", label)
	}
	multiplier := uint64(1)
	switch match[2] {
	case "s":
		multiplier = 1000
	case "m":
		multiplier = 60 * 1000
	case "h":
		multiplier = 60 * 60 * 1000
	case "d":
		multiplier = 24 * 60 * 60 * 1000
	}
	if value > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("%s is outside the supported range", label)
	}
	milliseconds := int64(value * multiplier)
	if milliseconds < minValue || milliseconds > maxValue {
		return 0, fmt.Errorf("%s must be in [%d,%d] milliseconds", label, minValue, maxValue)
	}
	return milliseconds, nil
}
