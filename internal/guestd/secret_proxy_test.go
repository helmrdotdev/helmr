package guestd

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStageProtectedEnvPublicTrustAndCollisions(t *testing.T) {
	if _, err := os.Stat("/etc/ssl/certs/ca-certificates.crt"); err != nil {
		t.Skip("guest system CA bundle required; exercised in Linux fixture")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	selector := "hlmr_protected_" + strings.Repeat("a", 64)
	root := t.TempDir()
	env := []string{"LITERAL=value", "https_proxy=http://untrusted.invalid", "NO_PROXY=*"}
	if err := stageProtectedEnv(root, map[string]string{"GH_TOKEN": selector}, ca, &env); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"GH_TOKEN=" + selector, "LITERAL=value", "https_proxy=http://untrusted.invalid", "NO_PROXY=*", "NODE_EXTRA_CA_CERTS=" + guestSecretProxyRoot + "/ca.pem"} {
		found := false
		for _, value := range env {
			if value == entry {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing guest configuration %s", entry)
		}
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "HTTP_PROXY=") || strings.HasPrefix(entry, "HTTPS_PROXY=") || strings.HasPrefix(entry, "NODE_USE_ENV_PROXY=") {
			t.Fatal("Helmr injected process routing", entry)
		}
	}
	stored, err := os.ReadFile(filepath.Join(root, "run/helmr/secret-proxy/ca.pem"))
	if err != nil || !bytes.Equal(stored, ca) || bytes.Contains(stored, []byte("PRIVATE")) {
		t.Fatal("guest trust is not public-only")
	}
	collision := []string{"GH_TOKEN=literal"}
	if err := stageProtectedEnv(t.TempDir(), map[string]string{"GH_TOKEN": selector}, ca, &collision); err == nil {
		t.Fatal("image/exec env collision accepted")
	}
	private, _ := x509.MarshalPKCS8PrivateKey(key)
	bad := append(bytes.Clone(ca), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})...)
	empty := []string{}
	if err := stageProtectedEnv(t.TempDir(), map[string]string{"GH_TOKEN": selector}, bad, &empty); err == nil {
		t.Fatal("private key accepted in guest trust")
	}
	for _, path := range []string{"/workspace/key", "/run/helmr/key", "/var/lib/helmr/key"} {
		if validateProgramSecretFilePath(path) == nil {
			t.Fatalf("reserved file accepted %s", path)
		}
	}
}

func TestGuestSecretEnvNameContract(t *testing.T) {
	data, err := os.ReadFile("../workspace/testdata/secret-env-names.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name     string
		Reserved bool
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if reservedSecretEnv(c.Name) != c.Reserved {
			t.Errorf("%s: reserved mismatch", c.Name)
		}
	}
}
