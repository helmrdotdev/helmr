package auth

import (
	"bytes"
	"testing"
)

func TestKeysAreStableAndDomainSeparated(t *testing.T) {
	root := make([]byte, RootKeySize)
	for index := range root {
		root[index] = byte(index)
	}
	first, err := NewKeys(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewKeys(root)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Valid() || !second.Valid() {
		t.Fatal("derived keys are invalid")
	}
	values := [][]byte{
		first.Session,
		first.Invitation,
		first.WorkerEnrollment,
		first.WorkerInstance,
		first.MagicLink,
		first.DeviceCode,
		first.BrowserAuth,
		first.WorkspaceFileCursor,
		first.TelemetryCursor,
	}
	replayed := [][]byte{
		second.Session,
		second.Invitation,
		second.WorkerEnrollment,
		second.WorkerInstance,
		second.MagicLink,
		second.DeviceCode,
		second.BrowserAuth,
		second.WorkspaceFileCursor,
		second.TelemetryCursor,
	}
	for index := range values {
		if !bytes.Equal(values[index], replayed[index]) {
			t.Fatalf("domain %d is not stable", index)
		}
		for other := range values {
			if index != other && bytes.Equal(values[index], values[other]) {
				t.Fatalf("domains %d and %d share a key", index, other)
			}
		}
	}
	sessionMAC, err := MAC(first.Session, []byte("same-input"))
	if err != nil {
		t.Fatal(err)
	}
	invitationMAC, err := MAC(first.Invitation, []byte("same-input"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(sessionMAC, invitationMAC) {
		t.Fatal("cross-domain MAC replay was accepted")
	}
}

func TestKeysRequireExactRootSize(t *testing.T) {
	for _, size := range []int{0, RootKeySize - 1, RootKeySize + 1} {
		if _, err := NewKeys(make([]byte, size)); err == nil {
			t.Fatalf("accepted %d-byte root", size)
		}
	}
}

func TestMACFramesOrderedInputs(t *testing.T) {
	key := make([]byte, MACKeySize)
	first, err := MAC(key, []byte("ab"), []byte("c"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := MAC(key, []byte("a"), []byte("bc"))
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := MAC(key, []byte("c"), []byte("ab"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) || bytes.Equal(first, reordered) {
		t.Fatal("MAC input framing is ambiguous")
	}
}
