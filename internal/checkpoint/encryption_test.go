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

func TestEncryptorRejectsTruncatedCiphertext(t *testing.T) {
	encryptor, err := New(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	if err := encryptor.Encrypt(context.Background(), strings.NewReader("state"), &encrypted, "vmstate"); err != nil {
		t.Fatal(err)
	}
	ciphertext := encrypted.Bytes()
	if len(ciphertext) < 8 {
		t.Fatalf("encrypted checkpoint too short: %d", len(ciphertext))
	}
	var decrypted bytes.Buffer
	err = encryptor.Decrypt(context.Background(), bytes.NewReader(ciphertext[:len(ciphertext)-8]), &decrypted, "vmstate")
	if err == nil {
		t.Fatal("expected decrypt failure for truncated checkpoint")
	}
}

func TestEncryptorRejectsTrailingCiphertext(t *testing.T) {
	encryptor, err := New(bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	if err := encryptor.Encrypt(context.Background(), strings.NewReader("state"), &encrypted, "vmstate"); err != nil {
		t.Fatal(err)
	}
	ciphertext := append(encrypted.Bytes(), 0)
	var decrypted bytes.Buffer
	err = encryptor.Decrypt(context.Background(), bytes.NewReader(ciphertext), &decrypted, "vmstate")
	if err == nil {
		t.Fatal("expected decrypt failure for trailing ciphertext")
	}
}

func TestEncryptorRoundTripEmptyPlaintext(t *testing.T) {
	encryptor, err := New(bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	if err := encryptor.Encrypt(context.Background(), strings.NewReader(""), &encrypted, "vmstate"); err != nil {
		t.Fatal(err)
	}
	var decrypted bytes.Buffer
	if err := encryptor.Decrypt(context.Background(), bytes.NewReader(encrypted.Bytes()), &decrypted, "vmstate"); err != nil {
		t.Fatal(err)
	}
	if decrypted.Len() != 0 {
		t.Fatalf("decrypted length = %d", decrypted.Len())
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
