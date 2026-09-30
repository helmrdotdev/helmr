package dispatch

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

func queueScopeLockKey(environmentID pgtype.UUID, queueName string, concurrencyKey pgtype.Text) (int64, error) {
	if !environmentID.Valid {
		return 0, errors.New("environment ID is required")
	}
	if strings.TrimSpace(queueName) == "" {
		return 0, errors.New("queue name is required")
	}
	if concurrencyKey.Valid && concurrencyKey.String == "" {
		return 0, errors.New("persisted concurrency key cannot be empty")
	}

	queue := []byte(queueName)
	concurrency := []byte{}
	if concurrencyKey.Valid {
		concurrency = []byte(concurrencyKey.String)
	}
	body := make([]byte, 0, len("helmr.lock.queue.v0\x00")+16+8+len(queue)+len(concurrency))
	body = append(body, "helmr.lock.queue.v0\x00"...)
	body = appendLengthPrefixed(body, environmentID.Bytes[:])
	body = appendLengthPrefixed(body, queue)
	body = appendLengthPrefixed(body, concurrency)
	digest := sha256.Sum256(body)
	return int64(binary.BigEndian.Uint64(digest[:8])), nil
}

func appendLengthPrefixed(dst, value []byte) []byte {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	dst = append(dst, length[:]...)
	return append(dst, value...)
}
