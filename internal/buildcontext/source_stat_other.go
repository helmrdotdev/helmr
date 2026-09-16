//go:build !linux && !darwin

package buildcontext

import (
	"os"
	"time"
)

func sourceChangeTime(os.FileInfo) (time.Time, bool) {
	return time.Time{}, false
}
