package dbtest

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/jackc/pgx/v5/pgconn"
)

func MustExec(t *testing.T, ctx context.Context, executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, query string, args ...any) {
	t.Helper()
	if _, err := executor.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func Digest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return sha256sum.FormatDigest(sum[:])
}

func Hash(seed string) []byte {
	sum := sha256.Sum256([]byte(seed))
	return sum[:]
}

func ShortID(id uuid.UUID) string {
	return strings.ReplaceAll(id.String(), "-", "")[20:]
}
