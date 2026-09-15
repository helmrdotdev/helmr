//go:build !linux && !darwin

package buildcontext

import (
	"errors"
	"os"
)

const nonblockFlag = 0

func normalizeEntry(*os.Root, string, os.FileInfo) error {
	return errors.New("build source capture is unsupported on this platform")
}
