//go:build linux

package firecracker

import fc "github.com/firecracker-microvm/firecracker-go-sdk"

type Machine struct {
	inner *fc.Machine
}

func Wrap(machine *fc.Machine) *Machine {
	return &Machine{inner: machine}
}

func Supported() bool {
	return true
}
