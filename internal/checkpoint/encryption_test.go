package checkpoint

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestEncryptorRoundTrip(t *testing.T) {
	encryptor, err := New(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte(strings.Repeat("checkpoint memory ", 512*1024))
	var encrypted bytes.Buffer
	if err := encryptor.Encrypt(context.Background(), bytes.NewReader(plaintext), &encrypted, "memory"); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted.Bytes(), []byte("checkpoint memory")) {
		t.Fatal("encrypted checkpoint contains plaintext")
	}
	var decrypted bytes.Buffer
	if err := encryptor.Decrypt(context.Background(), bytes.NewReader(encrypted.Bytes()), &decrypted, "memory"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted.Bytes(), plaintext) {
		t.Fatal("decrypted checkpoint did not match plaintext")
	}
}

func TestEncryptorRejectsWrongPurpose(t *testing.T) {
	encryptor, err := New(bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	if err := encryptor.Encrypt(context.Background(), strings.NewReader("state"), &encrypted, "vmstate"); err != nil {
		t.Fatal(err)
	}
	var decrypted bytes.Buffer
	if err := encryptor.Decrypt(context.Background(), bytes.NewReader(encrypted.Bytes()), &decrypted, "memory"); err == nil {
		t.Fatal("expected decrypt failure for wrong purpose")
	}
}

func TestKeyFromBase64(t *testing.T) {
	key, err := KeyFromBase64("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 32 {
		t.Fatalf("len = %d", len(key))
	}
	if _, err := KeyFromBase64("short"); err == nil {
		t.Fatal("expected invalid key")
	}
}
