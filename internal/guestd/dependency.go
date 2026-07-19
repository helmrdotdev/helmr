package guestd

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

type dependencyComponent struct {
	artifact deployment.ManagerArtifact
	device   string
	name     string
	noexec   bool
}

func handleDependencyConnection(ctx context.Context, conn io.ReadWriteCloser) (retErr error) {
	request, err := deployment.ReadManagerRequest(ctx, conn)
	if err != nil {
		return err
	}
	staged, err := stageDependencyComponents(ctx, request)
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, staged.Close())
	}()
	return fmt.Errorf("dependency manager %q execution is unavailable", request.Operation)
}

func dependencyComponents(request deployment.ManagerRequest) []dependencyComponent {
	components := []dependencyComponent{
		{
			artifact: request.ManagerTree,
			device:   "/dev/vdc",
			name:     "manager",
		},
		{
			artifact: request.Runtime,
			device:   "/dev/vdd",
			name:     "runtime",
		},
		{
			artifact: request.StandardToolchain,
			device:   "/dev/vde",
			name:     "toolchain",
		},
	}
	if request.Project != nil {
		components = append(components, dependencyComponent{
			artifact: *request.Project,
			device:   "/dev/vdf",
			name:     "project",
			noexec:   true,
		})
	}
	if request.OfflineStore != nil {
		components = append(components, dependencyComponent{
			artifact: *request.OfflineStore,
			device:   "/dev/vdg",
			name:     "offline-store",
			noexec:   true,
		})
	}
	return components
}

type stagedDependencyComponents interface {
	Close() error
}

func verifyDependencyContent(
	ctx context.Context,
	source io.Reader,
	size int64,
	digest string,
) error {
	if ctx == nil {
		return errors.New("dependency component context is nil")
	}
	if source == nil {
		return errors.New("dependency component source is nil")
	}
	if size <= 0 {
		return fmt.Errorf("dependency component size = %d", size)
	}
	encoded, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(encoded) != sha256.Size*2 ||
		encoded != strings.ToLower(encoded) {
		return errors.New("dependency component digest is not a lowercase SHA-256 digest")
	}
	expected, err := hex.DecodeString(encoded)
	if err != nil {
		return errors.New("dependency component digest is not a lowercase SHA-256 digest")
	}
	hasher := sha256.New()
	buffer := make([]byte, 128<<10)
	remaining := size
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		length := int64(len(buffer))
		if remaining < length {
			length = remaining
		}
		if _, err := io.ReadFull(source, buffer[:int(length)]); err != nil {
			return fmt.Errorf("read dependency component: %w", err)
		}
		_, _ = hasher.Write(buffer[:int(length)])
		remaining -= length
	}
	actual := hasher.Sum(nil)
	if subtle.ConstantTimeCompare(actual, expected) != 1 {
		return fmt.Errorf(
			"dependency component digest = sha256:%s, want %s",
			hex.EncodeToString(actual),
			digest,
		)
	}
	return nil
}
