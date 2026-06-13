//go:build linux

package firecracker

import "github.com/firecracker-microvm/firecracker-go-sdk"

type Machine struct {
	inner *firecracker.Machine
}

func Wrap(machine *firecracker.Machine) *Machine {
	return &Machine{inner: machine}
}

func Supported() bool {
	return true
}
