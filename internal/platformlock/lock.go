package platformlock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/pglock"
	"github.com/jackc/pgx/v5/pgxpool"
)

const lockDomain = "helmr.platform-artifact-lock.v0\x00"

type Locker struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) (*Locker, error) {
	if pool == nil {
		return nil, errors.New("platform artifact lock pool is required")
	}
	return &Locker{pool: pool}, nil
}

func (l *Locker) With(ctx context.Context, digests []string, fn func() error) error {
	if ctx == nil {
		return errors.New("platform artifact lock context is required")
	}
	if fn == nil {
		return errors.New("platform artifact lock function is required")
	}
	keys, err := lockKeys(digests)
	if err != nil {
		return err
	}
	guard, err := pglock.Acquire(ctx, l.pool, keys)
	if err != nil {
		return fmt.Errorf("acquire platform artifact lock: %w", err)
	}
	var runErr, unlockErr error
	func() {
		defer func() {
			unlockErr = guard.Unlock()
		}()
		runErr = fn()
	}()
	if unlockErr != nil {
		return errors.Join(runErr, fmt.Errorf("release platform artifact lock: %w", unlockErr))
	}
	return runErr
}

func lockKeys(values []string) ([]int64, error) {
	if len(values) == 0 {
		return nil, errors.New("at least one platform artifact digest is required")
	}
	digests := make([][sha256.Size]byte, 0, len(values))
	seenDigests := make(map[[sha256.Size]byte]struct{}, len(values))
	for _, value := range values {
		raw, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
		if err != nil || len(raw) != sha256.Size || value != "sha256:"+hex.EncodeToString(raw) {
			return nil, fmt.Errorf("platform artifact digest %q is invalid", value)
		}
		var digest [sha256.Size]byte
		copy(digest[:], raw)
		if _, seen := seenDigests[digest]; seen {
			continue
		}
		seenDigests[digest] = struct{}{}
		digests = append(digests, digest)
	}
	slices.SortFunc(digests, func(left, right [sha256.Size]byte) int {
		return bytes.Compare(left[:], right[:])
	})
	keys := make([]int64, 0, len(digests))
	seenKeys := make(map[int64]struct{}, len(digests))
	for _, digest := range digests {
		hash := sha256.New()
		_, _ = hash.Write([]byte(lockDomain))
		_, _ = hash.Write(digest[:])
		key := int64(binary.BigEndian.Uint64(hash.Sum(nil)[:8]))
		if _, seen := seenKeys[key]; seen {
			continue
		}
		seenKeys[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys, nil
}
