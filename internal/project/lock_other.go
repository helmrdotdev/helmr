//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package project

func lockCache(root string) (func(), error) {
	return func() {}, nil
}
