//go:build !linux

package guestd

import "errors"

func exchangeWorkspaceRoots(_, _ string) error {
	return errors.New("atomic Workspace root exchange is unsupported on this platform")
}
