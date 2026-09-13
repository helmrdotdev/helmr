package secret

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"slices"
	"testing"
	"time"
	"uuid"
)

type trustMemory struct {
	db.Querier
	row db.WorkspaceSecretProxyTrust
}

func (m *trustMemory) GetWorkspaceProxyTrust(context.Context, db.GetWorkspaceProxyTrustParams) (db.WorkspaceSecretProxyTrust, error) {
	if !m.row.WorkspaceID.Valid {
		return m.row, pgx.ErrNoRows
	}
	return m.row, nil
}
func (m *trustMemory) CreateWorkspaceProxyTrust(_ context.Context, p db.CreateWorkspaceProxyTrustParams) (db.WorkspaceSecretProxyTrust, error) {
	m.row = db.WorkspaceSecretProxyTrust(p)
	return m.row, nil
}

func TestProxyTrustCustodyLifetimeAndScope(t *testing.T) {
	store := &Store{encryption: testCipher(t)}
	memory := &trustMemory{}
	created := time.Now().Add(-time.Hour)
	root, err := store.EnsureProxyTrust(t.Context(), memory, uuid.NewV7(), uuid.NewV7(), created)
	if err != nil {
		t.Fatal(err)
	}
	if !root.NotAfter.Time.Equal(created.AddDate(10, 0, 0).Truncate(time.Second)) {
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
	if cert.NotAfter.After(time.Now().Add(24*time.Hour)) || cert.NotAfter.After(root.NotAfter.Time) {
		t.Fatal("leaf lifetime exceeded")
	}
	block, _ := pem.Decode(keyPEM)
	if bytes.Contains(root.PrivateKeyCiphertext, block.Bytes) || bytes.Contains(root.Certificate, []byte("PRIVATE")) {
		t.Fatal("private material was not encrypted")
	}
	other := root
	other.WorkspaceID.Bytes[0] ^= 1
	if _, _, err := store.ProxyLeaf(other, []string{"api.github.com"}); err == nil {
		t.Fatal("cross-Workspace signer decrypt succeeded")
	}
	if err := ValidateProxyTrust(root, root.NotAfter.Time); !errors.Is(err, ErrProxyTrustExpired) {
		t.Fatalf("expiry=%v", err)
	}
	again, err := store.EnsureProxyTrust(t.Context(), memory, pgvalue.MustUUIDValue(root.EnvironmentID), pgvalue.MustUUIDValue(root.WorkspaceID), created)
	if err != nil || !bytes.Equal(root.Certificate, again.Certificate) {
		t.Fatal("trust changed on reconnect")
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
	root, err := store.EnsureProxyTrust(t.Context(), &trustMemory{}, uuid.NewV7(), uuid.NewV7(), time.Now())
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
