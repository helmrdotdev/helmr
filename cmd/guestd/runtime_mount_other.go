//go:build !linux

package main

func mountImageRuntimeFilesystems(_ string) (func(), error) {
	return func() {}, nil
}
