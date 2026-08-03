package guestd

import (
	"os"
	"syscall"
)

func writeFileNoFollow(path string, body []byte, mode os.FileMode) error {
	file, err := os.OpenFile(
		path,
		os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW,
		mode,
	)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(body)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
