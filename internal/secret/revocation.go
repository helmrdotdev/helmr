package secret

import (
	"errors"
	"uuid"
)

// Revocation is one committed Secret revocation: the Secret, its
// Environment and the revocation generation it reached.
type Revocation struct {
	EnvironmentID uuid.UUID
	SecretID      uuid.UUID
	Generation    int64
}

// Validate reports whether the revocation names its Environment, its Secret
// and a positive generation.
func (r Revocation) Validate() error {
	if r.EnvironmentID == uuid.Nil() || r.SecretID == uuid.Nil() ||
		r.Generation <= 0 {
		return errors.New("secret revocation authority is required")
	}
	return nil
}
