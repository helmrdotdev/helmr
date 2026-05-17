//go:build !linux

package firecracker

type Machine struct{}

func Supported() bool {
	return false
}
