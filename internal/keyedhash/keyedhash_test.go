package keyedhash

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestKeyringUsesExactVersion(t *testing.T) {
	ring, err := New(2, map[int32][]byte{
		1: bytes.Repeat([]byte{1}, KeySize),
		2: bytes.Repeat([]byte{2}, KeySize),
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := ring.Current([]byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	if current.Version != 2 {
		t.Fatalf("current version = %d, want 2", current.Version)
	}
	previous, err := ring.Sum(1, []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	if current.Value == previous.Value {
		t.Fatal("different keys produced the same digest")
	}
	all := ring.All([]byte("value"))
	if len(all) != 2 || all[0].Version != 2 || all[1].Version != 1 {
		t.Fatalf("all = %+v", all)
	}
}

func TestKeyringListsCurrentFirst(t *testing.T) {
	ring, err := New(1, map[int32][]byte{
		1: bytes.Repeat([]byte{1}, KeySize),
		2: bytes.Repeat([]byte{2}, KeySize),
		3: bytes.Repeat([]byte{3}, KeySize),
	})
	if err != nil {
		t.Fatal(err)
	}
	all := ring.All([]byte("value"))
	if len(all) != 3 || all[0].Version != 1 || all[1].Version != 3 || all[2].Version != 2 {
		t.Fatalf("all = %+v", all)
	}
}

func TestKeyringParsesVersionedJSON(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, KeySize))
	ring, err := FromBase64JSON(`{"current":3,"keys":{"3":"` + encoded + `"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if ring.CurrentVersion() != 3 {
		t.Fatalf("current version = %d", ring.CurrentVersion())
	}
}

func TestKeyringRejectsInvalidConfiguration(t *testing.T) {
	if _, err := New(0, map[int32][]byte{1: bytes.Repeat([]byte{1}, KeySize)}); err == nil {
		t.Fatal("expected invalid current version error")
	}
	if _, err := New(1, map[int32][]byte{1: []byte("short")}); err == nil {
		t.Fatal("expected invalid key length error")
	}
	if _, err := New(2, map[int32][]byte{1: bytes.Repeat([]byte{1}, KeySize)}); err == nil {
		t.Fatal("expected missing current key error")
	}
}
