//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package projectconfig

func lockCache(root string) (func(), error) {
	return func() {}, nil
}
