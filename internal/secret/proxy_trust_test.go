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
	certPEM, keyPEM, err := store.ProxyLeaf(root, []string{"api.github.com"})
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
	other.WorkspaceID = uuid.NewV7()
	if _, _, err := store.ProxyLeaf(other, []string{"api.github.com"}); err == nil {
		t.Fatal("cross-Workspace signer decrypt succeeded")
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
	certificate, key, err := store.ProxyLeaf(root, hosts)
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
	if _, _, err := store.ProxyLeaf(root, append(hosts, "extra.example.com")); err == nil {
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
