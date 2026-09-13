package secret

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"regexp"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

var ErrProxyTrustExpired = errors.New("workspace Secret transport has expired; create a new Workspace")

func proxyTrustAAD(environmentID, workspaceID uuid.UUID) []byte {
	return []byte("helmr.workspace-secret-proxy-trust.v0\x00" + environmentID.String() + "\x00" + workspaceID.String())
}

// EnsureProxyTrust keeps only the encrypted Workspace signer in durable storage.
// The single-winner upsert is independent of execution authority; no grant is created here.
func (s *Store) EnsureProxyTrust(ctx context.Context, q db.Querier, environmentID, workspaceID uuid.UUID, createdAt time.Time) (db.WorkspaceSecretProxyTrust, error) {
	params := db.GetWorkspaceProxyTrustParams{EnvironmentID: pgvalue.UUID(environmentID), WorkspaceID: pgvalue.UUID(workspaceID)}
	row, err := q.GetWorkspaceProxyTrust(ctx, params)
	if err == nil {
		return row, ValidateProxyTrust(row, time.Now())
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return row, err
	}
	expires := createdAt.AddDate(10, 0, 0).Truncate(time.Second)
	if !time.Now().Before(expires) {
		return row, ErrProxyTrustExpired
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return row, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return row, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Helmr Workspace Secret transport"},
		NotBefore: createdAt.Add(-5 * time.Minute), NotAfter: expires, IsCA: true, BasicConstraintsValid: true,
		MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return row, err
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return row, err
	}
	defer clear(private)
	nonce := make([]byte, s.encryption.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return row, err
	}
	ciphertext := s.encryption.Seal(nil, nonce, private, proxyTrustAAD(environmentID, workspaceID))
	return q.CreateWorkspaceProxyTrust(ctx, db.CreateWorkspaceProxyTrustParams{EnvironmentID: params.EnvironmentID, WorkspaceID: params.WorkspaceID,
		Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), PrivateKeyNonce: nonce, PrivateKeyCiphertext: ciphertext, NotAfter: pgvalue.Timestamptz(expires)})
}

func ValidateProxyTrust(row db.WorkspaceSecretProxyTrust, now time.Time) error {
	block, _ := pem.Decode(row.Certificate)
	if block == nil {
		return ErrDeliveryUnavailable
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !cert.IsCA || !row.NotAfter.Valid || !cert.NotAfter.Equal(row.NotAfter.Time) {
		return ErrDeliveryUnavailable
	}
	if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return ErrProxyTrustExpired
	}
	return nil
}

func (s *Store) ProxyLeaf(row db.WorkspaceSecretProxyTrust, hosts []string) ([]byte, []byte, error) {
	if len(hosts) == 0 || len(hosts) > 256 {
		return nil, nil, ErrDeliveryUnavailable
	}
	if err := ValidateProxyTrust(row, time.Now()); err != nil {
		return nil, nil, err
	}
	environmentID := pgvalue.MustUUIDValue(row.EnvironmentID)
	workspaceID := pgvalue.MustUUIDValue(row.WorkspaceID)
	private, err := s.encryption.Open(nil, row.PrivateKeyNonce, row.PrivateKeyCiphertext, proxyTrustAAD(environmentID, workspaceID))
	if err != nil {
		return nil, nil, ErrDeliveryUnavailable
	}
	defer clear(private)
	parsed, err := x509.ParsePKCS8PrivateKey(private)
	if err != nil {
		return nil, nil, ErrDeliveryUnavailable
	}
	signer, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, ErrDeliveryUnavailable
	}
	block, _ := pem.Decode(row.Certificate)
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, ErrDeliveryUnavailable
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	expires := now.Add(24 * time.Hour)
	if expires.After(root.NotAfter) {
		expires = root.NotAfter
	}
	leaf := &x509.Certificate{SerialNumber: serial, NotBefore: now.Add(-time.Minute), NotAfter: expires, DNSNames: hosts,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, signer)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	defer clear(keyDER)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

var protectedSelector = regexp.MustCompile(`^hlmr_protected_[0-9a-f]{64}$`)

func ValidateProtectedSelectors(selectors []string) error {
	if len(selectors) == 0 || len(selectors) > 64 {
		return ErrDeliveryUnavailable
	}
	seen := make(map[string]bool, len(selectors))
	for _, selector := range selectors {
		if !protectedSelector.MatchString(selector) || seen[selector] {
			return ErrDeliveryUnavailable
		}
		seen[selector] = true
	}
	return nil
}

// OpenProtected consumes only envelopes captured by one authorized primary
// statement. Later rotation/revocation may overlap this already-authorized use;
// no mutable database read may select a replacement version during decryption.
func (s *Store) OpenProtected(rows []db.CaptureProtectedSecretEnvelopesRow, selectors []string) (map[string][]byte, error) {
	if err := ValidateProtectedSelectors(selectors); err != nil || len(rows) != len(selectors) {
		return nil, ErrDeliveryUnavailable
	}
	expected := make(map[string]bool, len(selectors))
	for _, selector := range selectors {
		expected[selector] = true
	}
	// Validate complete coverage and public trust before decrypting any value.
	for _, row := range rows {
		if !expected[row.Placeholder] || !row.EnvironmentID.Valid || !row.SecretID.Valid || !row.VersionID.Valid || !row.AuthorizedAt.Valid {
			return nil, ErrDeliveryUnavailable
		}
		delete(expected, row.Placeholder)
		if err := ValidateProxyTrust(db.WorkspaceSecretProxyTrust{Certificate: row.Certificate, NotAfter: row.NotAfter}, row.AuthorizedAt.Time); err != nil {
			return nil, err
		}
	}
	values := make(map[string][]byte, len(rows))
	success := false
	defer func() {
		if !success {
			for _, value := range values {
				clear(value)
			}
		}
	}()
	for _, row := range rows {
		value, err := s.decryptVersion(pgvalue.MustUUIDValue(row.EnvironmentID), pgvalue.MustUUIDValue(row.SecretID),
			pgvalue.MustUUIDValue(row.VersionID), db.SecretVersion{Version: row.Version, Nonce: row.Nonce, Ciphertext: row.Ciphertext})
		if err != nil {
			return nil, ErrDeliveryUnavailable
		}
		values[row.Placeholder] = value
	}
	success = true
	return values, nil
}
