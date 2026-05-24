package guestd

import "os"

const defaultGuestdTempRoot = "/var/lib/helmr/tmp"

func mkdirGuestdTemp(prefix string) (string, error) {
	root := os.Getenv("HELMR_GUESTD_TMPDIR")
	if root == "" {
		root = defaultGuestdTempRoot
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(root, prefix)
}
