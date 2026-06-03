//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package adapter

import "os"

func temporaryCacheRoot() (string, error) {
	return os.MkdirTemp("", "helmr-adapter-cache-*")
}

func ensurePrivateDir(root string) error {
	return os.MkdirAll(root, 0o700)
}
