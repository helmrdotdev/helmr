package guestd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

func handleManagerAcquire(conn io.ReadWriter, bodyLen uint64) error {
	if bodyLen == 0 || bodyLen > deployment.ManagerAcquireMaxInputBytes {
		return fmt.Errorf("manager acquisition input size %d is invalid", bodyLen)
	}
	root, err := mkdirGuestdTemp("helmr-manager-*")
	if err != nil {
		return fmt.Errorf("create manager acquisition scratch: %w", err)
	}
	defer os.RemoveAll(root)

	archive, err := os.OpenFile(
		filepath.Join(root, "source"),
		os.O_RDWR|os.O_CREATE|os.O_EXCL,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("create manager acquisition archive: %w", err)
	}
	defer archive.Close()

	input := &io.LimitedReader{R: conn, N: int64(bodyLen)}
	request, err := deployment.ReadManagerAcquireRequest(input, archive)
	if err != nil {
		return err
	}
	if input.N != 0 {
		return errors.New("manager acquisition request has trailing input")
	}
	return deployment.NormalizeManagerArchive(conn, request, archive, root)
}
