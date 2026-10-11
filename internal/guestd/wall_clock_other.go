//go:build !linux

package guestd

import (
	"errors"
	"time"
)

func setGuestWallClock(time.Time) error {
	return errors.New("guest wall clock synchronization requires Linux")
}
