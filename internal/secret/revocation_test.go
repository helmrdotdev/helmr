package secret

import (
	"testing"
	"uuid"
)

func TestRevocationValidateRequiresAuthority(t *testing.T) {
	valid := Revocation{EnvironmentID: uuid.NewV7(), SecretID: uuid.NewV7(), Generation: 1}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*Revocation){
		"environment": func(r *Revocation) { r.EnvironmentID = uuid.Nil() },
		"secret":      func(r *Revocation) { r.SecretID = uuid.Nil() },
		"generation":  func(r *Revocation) { r.Generation = 0 },
	} {
		invalid := valid
		edit(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Fatalf("revocation without %s validated", name)
		}
	}
}
