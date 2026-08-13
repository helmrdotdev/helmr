//go:build !darwin && !linux

package builder

import "errors"

func publishBundleDirectory(string, string) error {
	return errors.New("atomic no-replace bundle publication is unsupported")
}
