package idempotency

import (
	"fmt"
	"testing"
	"uuid"
)

func TestPlatformScopeAndCanonicalPayload(t *testing.T) {
	env, computer := uuid.NewV7(), uuid.NewV7()
	a, err := NewComputerCommandRequest(env, computer, "key", ComputerCommandFingerprint{Command: []string{"echo", "ok"}, Env: []byte(`{"B":"2","A":"1"}`), TimeoutMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewComputerCommandRequest(env, computer, "key", ComputerCommandFingerprint{Command: []string{"echo", "ok"}, Env: []byte(`{"A":"1","B":"2"}`), TimeoutMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	af := a.fingerprint
	bf := b.fingerprint
	if af != bf || idempotencySlotHash(a) != idempotencySlotHash(b) {
		t.Fatal("JSON key order changed retry identity")
	}
	other := *a
	otherID := uuid.NewV7()
	other.scope = otherID[:]
	if idempotencySlotHash(&other) == idempotencySlotHash(a) {
		t.Fatal("Computer scope omitted")
	}
	first, _ := NewSecretCreateRequest(env, "FIRST", "key")
	second, _ := NewSecretCreateRequest(env, "SECOND", "key")
	if idempotencySlotHash(first) == idempotencySlotHash(second) {
		t.Fatal("Secret name scope omitted")
	}
}

func TestSlotHashFramesEveryAuthorityField(t *testing.T) {
	base := Request{environmentID: uuid.NewV7(), operation: operationSecretCreate, scope: []byte("ab"), key: "c"}
	want := idempotencySlotHash(&base)
	for _, variant := range []Request{
		{environmentID: uuid.NewV7(), operation: base.operation, scope: base.scope, key: base.key},
		{environmentID: base.environmentID, operation: operationSecretRotate, scope: base.scope, key: base.key},
		{environmentID: base.environmentID, operation: base.operation, scope: []byte("a"), key: "bc"},
	} {
		if idempotencySlotHash(&variant) == want {
			t.Fatal("retry namespace collision")
		}
	}
}

func TestComputerCreateRetainsExternalScopeHash(t *testing.T) {
	env := uuid.UUID{0, 0, 0, 0, 0, 0, 0x70, 0, 0x80, 0, 0, 0, 0, 0, 0, 1}
	request, err := NewExternalComputerCreateRequest(env, "machine", "retry", ComputerCreateFingerprint{})
	if err != nil {
		t.Fatal(err)
	}
	if string(request.scope) != "external\x00machine" {
		t.Fatalf("scope: %q", request.scope)
	}
	if got := fmt.Sprintf("%x", idempotencySlotHash(request)); got != "bd0806e143a8907c3153e2fc9c1597b1e140440001c9f3d7db5d8aa06cf5dd62" {
		t.Fatalf("scope hash changed: %s", got)
	}
	claim, err := newClaim(request, idempotencySlotHash(request))
	if err != nil || claim.ScopeComputerKey.String != "machine" {
		t.Fatal(claim, err)
	}
}
