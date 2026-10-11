package cas

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"io"
	"strings"
	"syscall"
	"testing"
)

type failedObjectReader struct{}

func (failedObjectReader) Read([]byte) (int, error) { return 0, syscall.ECONNRESET }

func TestVerifyingReaderDoesNotHashIncompleteTransport(t *testing.T) {
	for _, consume := range []bool{false, true} {
		reader := NewVerifyingReadCloser(io.NopCloser(io.MultiReader(strings.NewReader("prefix"), failedObjectReader{})), sha256sum.DigestBytes([]byte("prefix-rest")))
		if consume {
			_, err := io.Copy(io.Discard, reader)
			if !errors.Is(err, syscall.ECONNRESET) || errors.Is(err, ErrDigestMismatch) {
				t.Fatalf("read: %v", err)
			}
		}
		for range 2 {
			err := reader.Close()
			if !errors.Is(err, syscall.ECONNRESET) || errors.Is(err, ErrDigestMismatch) {
				t.Fatalf("close: %v", err)
			}
		}
	}
}
func TestVerifyingReaderStillRejectsCompleteWrongContents(t *testing.T) {
	reader := NewVerifyingReadCloser(io.NopCloser(strings.NewReader("other")), sha256sum.DigestBytes([]byte("expected")))
	if err := reader.Close(); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("close: %v", err)
	}
}
