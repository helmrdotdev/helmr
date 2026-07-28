package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"os"
	"testing"
)

func TestVerifyPackageManagerIntegrityBindsDistribution(t *testing.T) {
	raw := []byte("manager distribution")
	digest := sha256.Sum256(raw)
	file, err := os.CreateTemp(t.TempDir(), "manager-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(raw); err != nil {
		t.Fatal(err)
	}
	integrity := "sha256." + hex.EncodeToString(digest[:])
	if err := verifyPackageManagerIntegrity(file, integrity); err != nil {
		t.Fatal(err)
	}
	if err := verifyPackageManagerIntegrity(
		file,
		"sha256."+hex.EncodeToString(make([]byte, sha256.Size)),
	); err == nil {
		t.Fatal("mismatched Manager distribution integrity was accepted")
	}
	ssriDigest := sha512.Sum512(raw)
	ssri := "sha512-" + base64.StdEncoding.EncodeToString(ssriDigest[:])
	if err := verifyRetainedRegistryDistribution(
		context.Background(),
		bytes.NewReader(raw),
		int64(len(raw)),
		ssri,
		integrity,
	); err != nil {
		t.Fatal(err)
	}
	if err := verifyRetainedRegistryDistribution(
		context.Background(),
		bytes.NewReader([]byte("tampered distribution")),
		int64(len("tampered distribution")),
		ssri,
		integrity,
	); err == nil {
		t.Fatal("tampered retained Manager distribution was accepted")
	}
}
