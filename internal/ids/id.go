package ids

import (
	"errors"
	"uuid"
)

var ErrInvalid = errors.New("invalid UUIDv7")

func Parse(value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil ||
		id == uuid.Nil() ||
		id[6]>>4 != 7 ||
		id[8]&0xc0 != 0x80 ||
		id.String() != value {
		return uuid.Nil(), ErrInvalid
	}
	return id, nil
}

func Validate(value string) error {
	_, err := Parse(value)
	return err
}
