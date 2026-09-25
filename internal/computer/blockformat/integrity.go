package blockformat

import "errors"

// ErrIntegrity marks invalid authenticated content, not transport or cancellation.
// It does not identify whether the bytes came from local or published storage.
var ErrIntegrity = errors.New("generation content integrity failure")

func integrity(err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(ErrIntegrity, err)
}

// ErrAuthentication can mean corrupt ciphertext or unavailable/wrong key material.
// It stops an active device but alone cannot prove remote data loss.
var ErrAuthentication = errors.New("generation authentication failure")
