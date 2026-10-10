package computerhost

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
)

func TestCheckpointEncryptorRoundTrip(t *testing.T) {
	encryptor, err := NewCheckpointEncryptor(bytes.Repeat([]byte{1}, 32))
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

func TestCheckpointEncryptorRejectsWrongPurpose(t *testing.T) {
	encryptor, err := NewCheckpointEncryptor(bytes.Repeat([]byte{2}, 32))
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

func TestCheckpointEncryptorRejectsTruncatedCiphertext(t *testing.T) {
	encryptor, err := NewCheckpointEncryptor(bytes.Repeat([]byte{3}, 32))
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

func TestCheckpointEncryptorRejectsTrailingCiphertext(t *testing.T) {
	encryptor, err := NewCheckpointEncryptor(bytes.Repeat([]byte{4}, 32))
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

func TestCheckpointEncryptorRoundTripEmptyPlaintext(t *testing.T) {
	encryptor, err := NewCheckpointEncryptor(bytes.Repeat([]byte{4}, 32))
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

func TestEncryptedSizeMatchesActualFraming(t *testing.T) {
	cipher, err := NewCheckpointEncryptor(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 2 * chunkSize} {
		var out bytes.Buffer
		if err := cipher.Encrypt(t.Context(), bytes.NewReader(make([]byte, size)), &out, "size-test"); err != nil {
			t.Fatal(err)
		}
		want, err := cipher.EncryptedSize(size)
		if err != nil || int64(out.Len()) != want {
			t.Fatalf("plaintext=%d actual=%d calculated=%d error=%v", size, out.Len(), want, err)
		}
	}
	for _, size := range []int64{-1, math.MaxInt64, math.MaxInt64 - 100} {
		if _, err := cipher.EncryptedSize(size); err == nil {
			t.Fatalf("accepted %d", size)
		}
	}
	if _, err := (*CheckpointEncryptor)(nil).EncryptedSize(1); err == nil {
		t.Fatal("missing encryptor accepted")
	}
}

func TestCheckpointEncryptorRejectsInvalidNonceWithoutPanic(t *testing.T) {
	cipher, err := NewCheckpointEncryptor(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var invalid bytes.Buffer
	header := make([]byte, headerSize)
	copy(header, magic)
	copy(header[len(magic):], cipher.keyID[:])
	invalid.Write(header)
	if err := writeRecord(&invalid, []byte{1}, bytes.Repeat([]byte{0}, 16)); err != nil {
		t.Fatal(err)
	}
	if err := cipher.Decrypt(t.Context(), &invalid, &bytes.Buffer{}, "memory"); err == nil {
		t.Fatal("malformed nonce accepted")
	}
}

func TestCheckpointEncryptorSeparatesAttemptsAndAuthenticatesFraming(t *testing.T) {
	c, err := NewCheckpointEncryptor(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{42}, chunkSize+1)
	var first, second bytes.Buffer
	if err = c.Encrypt(t.Context(), bytes.NewReader(data), &first, "checkpoint/memory"); err != nil {
		t.Fatal(err)
	}
	if err = c.Encrypt(t.Context(), bytes.NewReader(data), &second, "checkpoint/memory"); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Bytes(), second.Bytes()) || bytes.Equal(first.Bytes()[len(magic)+keyIDSize:headerSize], second.Bytes()[len(magic)+keyIDSize:headerSize]) {
		t.Fatal("encryption attempts share a key domain")
	}
	reader := bytes.NewReader(first.Bytes()[headerSize:])
	var records [][]byte
	for reader.Len() > 0 {
		before := reader.Len()
		if _, _, err := readRecord(reader); err != nil {
			t.Fatal(err)
		}
		start := first.Len() - before
		records = append(records, append([]byte(nil), first.Bytes()[start:start+before-reader.Len()]...))
	}
	for _, fault := range []string{"key-id", "salt", "format", "reorder", "repeat", "missing-end", "end-before-data", "wrong-key", "wrong-purpose"} {
		t.Run(fault, func(t *testing.T) {
			raw := append([]byte(nil), first.Bytes()...)
			decryptor := c
			purpose := "checkpoint/memory"
			switch fault {
			case "key-id":
				raw[len(magic)] ^= 1
			case "salt":
				raw[headerSize-1] ^= 1
			case "format":
				raw[0] ^= 1
			case "reorder":
				raw = append(append(append(append([]byte(nil), raw[:headerSize]...), records[1]...), records[0]...), records[2]...)
			case "repeat":
				raw = append(append(append(append([]byte(nil), raw[:headerSize]...), records[0]...), records[0]...), records[2]...)
			case "missing-end":
				raw = raw[:len(raw)-len(records[2])]
			case "end-before-data":
				raw = append(append([]byte(nil), raw[:headerSize]...), records[2]...)
			case "wrong-key":
				decryptor, _ = NewCheckpointEncryptor(bytes.Repeat([]byte{8}, 32))
			case "wrong-purpose":
				purpose = "checkpoint/scratch_disk"
			}
			if err := decryptor.Decrypt(t.Context(), bytes.NewReader(raw), io.Discard, purpose); err == nil {
				t.Fatal("unauthenticated framing accepted")
			}
		})
	}
	if _, err := c.EncryptedSize(int64(maxRecords-1) * chunkSize); err != nil {
		t.Fatal(err)
	}
	if _, err := c.EncryptedSize(int64(maxRecords-1)*chunkSize + 1); err == nil {
		t.Fatal("end record nonce budget not reserved")
	}
}

func TestCheckpointKeyMismatchReportsOnlyFingerprints(t *testing.T) {
	key := bytes.Repeat([]byte{8}, 32)
	source, _ := NewCheckpointEncryptor(key)
	target, _ := NewCheckpointEncryptor(bytes.Repeat([]byte{9}, 32))
	var ciphertext bytes.Buffer
	if err := source.Encrypt(t.Context(), strings.NewReader("retained"), &ciphertext, "memory"); err != nil {
		t.Fatal(err)
	}
	err := target.Decrypt(t.Context(), &ciphertext, io.Discard, "memory")
	if !errors.Is(err, ErrCheckpointKeyUnavailable) || !strings.Contains(err.Error(), source.KeyID()) || !strings.Contains(err.Error(), target.KeyID()) || strings.Contains(err.Error(), hex.EncodeToString(key)) {
		t.Fatalf("key mismatch diagnostic: %v", err)
	}
}
