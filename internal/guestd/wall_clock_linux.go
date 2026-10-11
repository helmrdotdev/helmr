//go:build linux

package guestd

import (
	"time"

	"golang.org/x/sys/unix"
)

func setGuestWallClock(now time.Time) error {
	value := unix.Timespec{Sec: now.Unix(), Nsec: int64(now.Nanosecond())}
	return unix.ClockSettime(unix.CLOCK_REALTIME, &value)
}
