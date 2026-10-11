package idempotency

import (
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
	af, _ := a.idempotencyRequest().fingerprint()
	bf, _ := b.idempotencyRequest().fingerprint()
	if af != bf || idempotencySlotHash(a.idempotencyRequest()) != idempotencySlotHash(b.idempotencyRequest()) {
		t.Fatal("JSON key order changed retry identity")
	}
	other := a.idempotencyRequest()
	otherID := uuid.NewV7()
	other.scope = otherID[:]
	if idempotencySlotHash(other) == idempotencySlotHash(a.idempotencyRequest()) {
		t.Fatal("Computer scope omitted")
	}
	first, _ := NewSecretCreateRequest(env, "FIRST", "key")
	second, _ := NewSecretCreateRequest(env, "SECOND", "key")
	if idempotencySlotHash(first.idempotencyRequest()) == idempotencySlotHash(second.idempotencyRequest()) {
		t.Fatal("Secret name scope omitted")
	}
}

func TestSlotHashFramesEveryAuthorityField(t *testing.T) {
	base := request{environmentID: uuid.NewV7(), operation: operationSecretCreate, scope: []byte("ab"), key: "c"}
	want := idempotencySlotHash(base)
	for _, variant := range []request{
		{environmentID: uuid.NewV7(), operation: base.operation, scope: base.scope, key: base.key},
		{environmentID: base.environmentID, operation: operationSecretRotate, scope: base.scope, key: base.key},
		{environmentID: base.environmentID, operation: base.operation, scope: []byte("a"), key: "bc"},
	} {
		if idempotencySlotHash(variant) == want {
			t.Fatal("retry namespace collision")
		}
	}
}
