//go:build linux

package computerhost

import "github.com/helmrdotdev/helmr/internal/nbd"

func checkComputerDevicesIdle(devices []string) error {
	return nbd.VerifyIdleDevices(devices)
}
