package cas

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func ValidateDescriptor(expected Descriptor) error {
	hash, ok := strings.CutPrefix(expected.Digest, "sha256:")
	if !ok || len(hash) != sha256.Size*2 {
		return errors.New("immutable descriptor digest is not a lowercase SHA-256 digest")
	}
	for _, character := range hash {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') {
			return errors.New("immutable descriptor digest is not a lowercase SHA-256 digest")
		}
	}
	if expected.SizeBytes < 1 {
		return errors.New("immutable descriptor size must be positive")
	}
	if expected.MediaType == "" || strings.TrimSpace(expected.MediaType) != expected.MediaType {
		return errors.New("immutable descriptor media type is invalid")
	}
	return nil
}

func VerifyDescriptorFile(ctx context.Context, expected Descriptor, file *os.File) error {
	reader := io.NewSectionReader(file, 0, expected.SizeBytes+1)
	digest := sha256.New()
	buffer := make([]byte, 128<<10)
	var sizeBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("hash immutable file: %w", err)
		}
		count, readErr := reader.Read(buffer)
		if count > 0 {
			if _, err := digest.Write(buffer[:count]); err != nil {
				return fmt.Errorf("hash immutable file: %w", err)
			}
			sizeBytes += int64(count)
			if sizeBytes > expected.SizeBytes {
				return fmt.Errorf("immutable file exceeds expected size %d", expected.SizeBytes)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read immutable file: %w", readErr)
			}
			break
		}
		if count == 0 {
			return io.ErrNoProgress
		}
	}
	if sizeBytes != expected.SizeBytes {
		return fmt.Errorf("immutable file size = %d, want %d", sizeBytes, expected.SizeBytes)
	}
	actual := sha256sum.FormatDigest(digest.Sum(nil))
	if actual != expected.Digest {
		return fmt.Errorf("immutable file digest = %s, want %s", actual, expected.Digest)
	}
	return nil
}
