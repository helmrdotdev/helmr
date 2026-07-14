package control

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5/pgconn"
)

const publicIDCreateAttempts = 4

type publicIDSlot struct {
	prefix publicid.Prefix
	value  *string
}

func newPublicID(prefix publicid.Prefix) (string, error) {
	id, err := publicid.New(prefix)
	if err != nil {
		return "", fmt.Errorf("new public id: %w", err)
	}
	return id, nil
}

func createWithPublicID[T any](ctx context.Context, slots []publicIDSlot, create func() (T, error)) (T, error) {
	var zero T
	for range publicIDCreateAttempts {
		for _, slot := range slots {
			if slot.value == nil {
				return zero, errors.New("public id slot value is nil")
			}
			id, err := newPublicID(slot.prefix)
			if err != nil {
				return zero, err
			}
			*slot.value = id
		}
		row, err := create()
		if err == nil {
			return row, nil
		}
		if !isPublicIDUniqueViolation(err) {
			return zero, err
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
	}
	return zero, fmt.Errorf("create public id after %d attempts", publicIDCreateAttempts)
}

func isPublicIDUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "public_id")
}
