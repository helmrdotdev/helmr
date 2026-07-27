package control

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func parseRequiredUUID(field, raw string) (pgtype.UUID, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return pgtype.UUID{}, fmt.Errorf("%s is required", field)
	}
	id, err := uuid.Parse(trimmed)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%s must be a UUID", field)
	}
	return pgvalue.UUID(id), nil
}
