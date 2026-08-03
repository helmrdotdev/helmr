//go:build darwin

package archive

import (
	"os"
	"syscall"
	"time"
)

func sourceChangeTime(info os.FileInfo) (time.Time, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(stat.Ctimespec.Sec, stat.Ctimespec.Nsec), true
}
