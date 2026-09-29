package secret

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"regexp"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

var ErrProxyTrustExpired = errors.New("computer Secret transport has expired; create a new Computer")

// ErrWorkerClaimsStale refuses material captured for a Worker credential whose
// claim versions changed. It is a freshness signal: the Worker re-authenticates.
var ErrWorkerClaimsStale = errors.New("worker authentication claims are stale")

func proxyTrustAAD(environmentID, computerID uuid.UUID) []byte {
	return []byte("helmr.computer-secret-proxy-trust.v0\x00" + environmentID.String() + "\x00" + computerID.String())
}

// ProxyTrust is encrypted Computer CA material. Only creation and authorized
// preparation handle the signer; public delivery and resolution use certificate/expiry.
type ProxyTrust struct {
	EnvironmentID        uuid.UUID
	ComputerID           uuid.UUID
	Certificate          []byte
	PrivateKeyNonce      []byte
	PrivateKeyCiphertext []byte
	NotAfter             time.Time
}

// GenerateProxyTrust does not persist or retrieve material. Callers persist it
// only in the transaction that inserted the Computer, using that row's CreatedAt.
func (s *Store) GenerateProxyTrust(environmentID, computerID uuid.UUID, createdAt time.Time) (ProxyTrust, error) {
	row := ProxyTrust{EnvironmentID: environmentID, ComputerID: computerID}
	if s == nil || s.encryption == nil || createdAt.IsZero() || environmentID == uuid.Nil() || computerID == uuid.Nil() {
		return row, ErrDeliveryUnavailable
	}
	entropy := s.rand
	if entropy == nil {
		entropy = rand.Reader
	}
	expires := createdAt.AddDate(10, 0, 0).Truncate(time.Second)
	if !time.Now().Before(expires) {
		return row, ErrProxyTrustExpired
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), entropy)
	if err != nil {
		return row, err
	}
	serial, err := rand.Int(entropy, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return row, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Helmr Computer Secret transport"},
		NotBefore: createdAt.Add(-5 * time.Minute), NotAfter: expires, IsCA: true, BasicConstraintsValid: true,
		MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(entropy, template, template, &key.PublicKey, key)
	if err != nil {
		return row, err
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return row, err
	}
	defer clear(private)
	nonce := make([]byte, s.encryption.NonceSize())
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return row, err
	}
	ciphertext := s.encryption.Seal(nil, nonce, private, proxyTrustAAD(environmentID, computerID))
	row.Certificate = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	row.PrivateKeyNonce, row.PrivateKeyCiphertext, row.NotAfter = nonce, ciphertext, expires
	return row, nil
}

func ValidateProxyTrust(certificate []byte, notAfter, now time.Time) error {
	block, _ := pem.Decode(certificate)
	if block == nil {
		return ErrDeliveryUnavailable
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !cert.IsCA || notAfter.IsZero() || !cert.NotAfter.Equal(notAfter) {
		return ErrDeliveryUnavailable
	}
	if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return ErrProxyTrustExpired
	}
	return nil
}

// ProxyLeaf signs a leaf with the Computer CA captured by one authorized
// preparation statement, refusing a capture made with stale Worker claims.
func (s *Store) ProxyLeaf(captured db.CaptureSecretProxyPreparationRow, hosts []string) ([]byte, []byte, error) {
	if !captured.ClaimsCurrent {
		return nil, nil, ErrWorkerClaimsStale
	}
	environmentID, e1 := pgvalue.UUIDValue(captured.EnvironmentID)
	computerID, e2 := pgvalue.UUIDValue(captured.ComputerID)
	if e1 != nil || e2 != nil {
		return nil, nil, ErrDeliveryUnavailable
	}
	return s.proxyLeaf(ProxyTrust{
		EnvironmentID: environmentID, ComputerID: computerID,
		Certificate: captured.Certificate, NotAfter: captured.NotAfter.Time,
		PrivateKeyNonce: captured.PrivateKeyNonce, PrivateKeyCiphertext: captured.PrivateKeyCiphertext,
	}, hosts)
}

func (s *Store) proxyLeaf(row ProxyTrust, hosts []string) ([]byte, []byte, error) {
	if len(hosts) == 0 || len(hosts) > 256 {
		return nil, nil, ErrDeliveryUnavailable
	}
	if err := ValidateProxyTrust(row.Certificate, row.NotAfter, time.Now()); err != nil {
		return nil, nil, err
	}
	environmentID := row.EnvironmentID
	computerID := row.ComputerID
	if s == nil || s.encryption == nil || len(row.PrivateKeyNonce) != s.encryption.NonceSize() || len(row.PrivateKeyCiphertext) == 0 {
		return nil, nil, ErrDeliveryUnavailable
	}
	private, err := s.encryption.Open(nil, row.PrivateKeyNonce, row.PrivateKeyCiphertext, proxyTrustAAD(environmentID, computerID))
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
	for _, row := range rows {
		if !row.ClaimsCurrent {
			return nil, ErrWorkerClaimsStale
		}
	}
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
		if err := ValidateProxyTrust(row.Certificate, row.NotAfter.Time, row.AuthorizedAt.Time); err != nil {
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
