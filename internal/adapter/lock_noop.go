//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package adapter

func lockCache(root string) (func(), error) {
	return func() {}, nil
}
