package ids

import (
	"errors"

	"github.com/google/uuid"
)

var ErrInvalid = errors.New("invalid UUIDv7")

func Parse(value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil ||
		id == uuid.Nil ||
		id.Version() != uuid.Version(7) ||
		id.Variant() != uuid.RFC4122 ||
		id.String() != value {
		return uuid.Nil, ErrInvalid
	}
	return id, nil
}

func Validate(value string) error {
	_, err := Parse(value)
	return err
}
