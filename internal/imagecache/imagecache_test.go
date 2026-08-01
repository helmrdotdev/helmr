package imagecache

import (
	"errors"
	"testing"
)

func TestCredentialClearAndUnavailableClassification(t *testing.T) {
	credential := Credential{Password: []byte("secret")}
	buffer := credential.Password
	credential.Clear()
	if credential.Password != nil {
		t.Fatal("credential password was retained")
	}
	for _, value := range buffer {
		if value != 0 {
			t.Fatalf("credential buffer = %x", buffer)
		}
	}

	cause := errors.New("unavailable")
	err := &UnavailableError{Operation: "test", Err: cause}
	if !IsUnavailable(err) || !errors.Is(err, cause) {
		t.Fatalf("error = %T %v", err, err)
	}
	if IsUnavailable(&ContractError{Message: "mismatch"}) {
		t.Fatal("contract mismatch classified as unavailable")
	}
}
