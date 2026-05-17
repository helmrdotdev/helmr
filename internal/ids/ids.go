package ids

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

var DefaultOrgID = uuid.MustParse("00000000-0000-0000-0000-000000000000")

func New() uuid.UUID {
	return uuid.Must(uuid.NewV7())
}

func Parse(value string) (uuid.UUID, error) {
	return uuid.Parse(value)
}

func ToPG(value uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: value, Valid: true}
}

func FromPG(value pgtype.UUID) (uuid.UUID, error) {
	if !value.Valid {
		return uuid.Nil, fmt.Errorf("uuid is null")
	}
	return uuid.UUID(value.Bytes), nil
}

func MustFromPG(value pgtype.UUID) uuid.UUID {
	id, err := FromPG(value)
	if err != nil {
		panic(err)
	}
	return id
}
