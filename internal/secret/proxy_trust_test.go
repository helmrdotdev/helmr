package secret

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/db"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"
)

func TestProxyTrustCustodyLifetimeAndScope(t *testing.T) {
	store := &Store{encryption: testCipher(t)}
	created := time.Now().Add(-time.Hour)
	root, err := store.GenerateProxyTrust(uuid.NewV7(), uuid.NewV7(), created)
	if err != nil {
		t.Fatal(err)
	}
	if !root.NotAfter.Equal(created.AddDate(10, 0, 0).Truncate(time.Second)) {
		t.Fatal("root lifetime differs")
	}
	certPEM, keyPEM, err := store.proxyLeaf(root, []string{"api.github.com"})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(root.Certificate)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "api.github.com"}); err != nil {
		t.Fatal(err)
	}
	if cert.NotAfter.After(time.Now().Add(24*time.Hour)) || cert.NotAfter.After(root.NotAfter) {
		t.Fatal("leaf lifetime exceeded")
	}
	block, _ := pem.Decode(keyPEM)
	if bytes.Contains(root.PrivateKeyCiphertext, block.Bytes) || bytes.Contains(root.Certificate, []byte("PRIVATE")) {
		t.Fatal("private material was not encrypted")
	}
	other := root
	other.ComputerID = uuid.NewV7()
	if _, _, err := store.proxyLeaf(other, []string{"api.github.com"}); err == nil {
		t.Fatal("cross-Computer signer decrypt succeeded")
	}
	if err := ValidateProxyTrust(root.Certificate, root.NotAfter, root.NotAfter); !errors.Is(err, ErrProxyTrustExpired) {
		t.Fatalf("expiry=%v", err)
	}

}

func TestProtectedEnvelopeNeverDecryptsForGuest(t *testing.T) {
	// Deliberately invalid ciphertext and an absent cipher prove this branch cannot
	// decrypt protected material or produce raw frames.
	store := &Store{}
	materials, err := store.OpenDeliveries(uuid.NewV7(), []DeliveryEnvelope{{Mode: "protected", Version: db.SecretVersion{Ciphertext: []byte("invalid")}}})
	if err != nil || len(materials) != 0 {
		t.Fatalf("protected raw delivery=%v %v", materials, err)
	}
}

func TestProxyLeafIssuesMaximumAdmittedHosts(t *testing.T) {
	store := &Store{encryption: testCipher(t)}
	root, err := store.GenerateProxyTrust(uuid.NewV7(), uuid.NewV7(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	hosts := make([]string, 256)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("h%d.example.com", i)
	}
	certificate, key, err := store.proxyLeaf(root, hosts)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(leaf.DNSNames, hosts) {
		t.Fatal("issuer lost admitted hosts")
	}
	if _, _, err := store.proxyLeaf(root, append(hosts, "extra.example.com")); err == nil {
		t.Fatal("issuer exceeded validated capacity")
	}
}

func TestProxyTrustGenerationFailureReturnsNoMaterial(t *testing.T) {
	store := &Store{encryption: testCipher(t), rand: strings.NewReader("")}
	trust, err := store.GenerateProxyTrust(uuid.NewV7(), uuid.NewV7(), time.Now())
	if err == nil || len(trust.Certificate) != 0 || len(trust.PrivateKeyCiphertext) != 0 || len(trust.PrivateKeyNonce) != 0 {
		t.Fatal("failed generation returned material")
	}
	var missing *Store
	if _, err := missing.GenerateProxyTrust(uuid.NewV7(), uuid.NewV7(), time.Now()); !errors.Is(err, ErrDeliveryUnavailable) {
		t.Fatalf("missing generator: %v", err)
	}
}

func TestProxyMaterialRefusesStaleWorkerClaims(t *testing.T) {
	store := &Store{encryption: testCipher(t)}
	root, err := store.GenerateProxyTrust(uuid.NewV7(), uuid.NewV7(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, key, err := store.ComputerProxyLeaf(root, []string{"api.github.com"}); err != nil || len(key) == 0 {
		t.Fatalf("authorized capture: %v", err)
	}
	selector := "hlmr_protected_" + strings.Repeat("a", 64)
	rows := []ProtectedCapture{{Placeholder: selector, ClaimsCurrent: false}}
	if values, err := store.OpenProtectedCapture(rows, []string{selector}); !errors.Is(err, ErrWorkerClaimsStale) || values != nil {
		t.Fatalf("stale capture opened material: %v", err)
	}
}

func TestPreparationProxyTrustSeparatesAttemptAndComputerCustody(t *testing.T) {
	store := &Store{encryption: testCipher(t)}
	now, deadline := time.Now(), time.Now().Add(15*time.Minute)
	env, attempt := uuid.NewV7(), uuid.NewV7()
	trust, err := store.GeneratePreparationProxyTrust(env, attempt, now, deadline)
	if err != nil {
		t.Fatal(err)
	}
	if trust.ComputerID != uuid.Nil() || trust.PreparationID != attempt || !trust.NotAfter.Equal(deadline.Truncate(time.Second)) {
		t.Fatal("incorrect preparation trust scope or expiry")
	}
	certificate, key, err := store.PreparationProxyLeaf(trust, []string{"api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.NotAfter.After(trust.NotAfter) {
		t.Fatal("leaf outlived attempt")
	}
	for _, mutate := range []func(*ProxyTrust){
		func(v *ProxyTrust) { v.EnvironmentID = uuid.NewV7() },
		func(v *ProxyTrust) { v.PreparationID = uuid.NewV7() },
		func(v *ProxyTrust) { v.ComputerID, v.PreparationID = attempt, uuid.Nil() },
		func(v *ProxyTrust) { v.ComputerID = attempt },
	} {
		wrong := trust
		mutate(&wrong)
		if _, value, err := store.proxyLeaf(wrong, []string{"api.example.com"}); err == nil {
			clear(value)
			t.Fatal("signer opened under wrong owner")
		}
	}
	computer, err := store.GenerateProxyTrust(env, attempt, now)
	if err != nil {
		t.Fatal(err)
	}
	computer.ComputerID, computer.PreparationID = uuid.Nil(), attempt
	if _, value, err := store.PreparationProxyLeaf(computer, []string{"api.example.com"}); err == nil {
		clear(value)
		t.Fatal("Computer signer opened as preparation")
	}
}
