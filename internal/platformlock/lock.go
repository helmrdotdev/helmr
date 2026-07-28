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
	"time"

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
	connection, err := l.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire platform artifact lock connection: %w", err)
	}
	// A checked-out session carrying an advisory lock must never return to the
	// pool. Mark it disposable until every acquired lock is proven released.
	discard := true
	defer func() {
		if discard {
			raw := connection.Hijack()
			_ = raw.Close(context.Background())
			return
		}
		connection.Release()
	}()

	acquired := make([]int64, 0, len(keys))
	for _, key := range keys {
		if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
			return fmt.Errorf("acquire platform artifact lock: %w", err)
		}
		acquired = append(acquired, key)
	}

	runErr := fn()
	unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for index := len(acquired) - 1; index >= 0; index-- {
		var unlocked bool
		if err := connection.QueryRow(
			unlockCtx,
			"SELECT pg_advisory_unlock($1)",
			acquired[index],
		).Scan(&unlocked); err != nil || !unlocked {
			if err == nil {
				err = errors.New("PostgreSQL did not release the session lock")
			}
			return errors.Join(runErr, fmt.Errorf("release platform artifact lock: %w", err))
		}
	}
	discard = false
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
