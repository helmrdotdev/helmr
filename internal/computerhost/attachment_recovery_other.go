//go:build !linux

package computerhost

import "errors"

func checkComputerDevicesIdle([]string) error {
	return errors.New("computer attachment recovery requires Linux")
}
