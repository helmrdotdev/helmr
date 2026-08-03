//go:build !linux && !darwin

package archive

import (
	"os"
	"time"
)

func sourceChangeTime(os.FileInfo) (time.Time, bool) {
	return time.Time{}, false
}
