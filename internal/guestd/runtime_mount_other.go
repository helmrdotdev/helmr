//go:build !linux

package guestd

func mountImageRuntimeFilesystems(_ string) (func(), error) {
	return func() {}, nil
}
