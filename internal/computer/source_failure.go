package computer

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"io/fs"
)

// SourceFailure is established only by reading the retained published source
// directly. A missing unpublished local object is not proof of published loss.
type SourceFailure struct{ Cause error }

func (e *SourceFailure) Error() string {
	return "published Computer source is unavailable: " + e.Cause.Error()
}
func (e *SourceFailure) Unwrap() error { return e.Cause }

func PublishedSourceFailure(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, blockformat.ErrIntegrity) {
		return &SourceFailure{Cause: err}
	}
	return err
}

// DeviceFailure stops a disk export whose contents cannot safely be read. It
// does not establish whether the unavailable bytes were ever published remotely.
type DeviceFailure struct{ Cause error }

func (e *DeviceFailure) Error() string          { return "Computer disk read failed: " + e.Cause.Error() }
func (e *DeviceFailure) Unwrap() error          { return e.Cause }
func (e *DeviceFailure) FatalDeviceError() bool { return true }
func deviceFailure(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, blockformat.ErrIntegrity) || errors.Is(err, blockformat.ErrAuthentication) {
		return &DeviceFailure{Cause: err}
	}
	return err
}
