package guestd

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"github.com/helmrdotdev/helmr/internal/wire"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const guestSecretProxyRoot = "/run/helmr/secret-proxy"

var errSecretEnvCollision = errors.New(wire.SecretEnvCollisionDiagnostic)

var protectedSelectorPattern = regexp.MustCompile(`^hlmr_protected_[0-9a-f]{64}$`)

func reservedSecretEnv(name string) bool {
	if strings.HasPrefix(name, "HELMR_") || isManagedRuntimeEnvKey(name) {
		return true
	}
	switch strings.ToUpper(name) {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "NODE_USE_ENV_PROXY", "NODE_USE_SYSTEM_CA":
		return true
	}
	return false
}

// This file contains public trust only. Private signer and leaf keys never cross
// the worker/guest protocol. The image root is ephemeral, outside /workspace.
func stageProtectedEnv(imageRoot string, selectors map[string]string, ca []byte, env *[]string) error {
	if len(selectors) == 0 {
		if len(ca) != 0 {
			return errors.New("unexpected Workspace Secret trust")
		}
		return nil
	}
	if len(selectors) > 64 || len(ca) > 16384 {
		return errors.New("invalid Workspace Secret transport")
	}
	block, rest := pem.Decode(ca)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("invalid Workspace Secret public trust")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !certificate.IsCA || time.Now().Before(certificate.NotBefore) || !time.Now().Before(certificate.NotAfter) {
		return errors.New("expired or invalid Workspace Secret public trust")
	}
	for name, selector := range selectors {
		if !validEnvironmentName(name) || reservedSecretEnv(name) || !protectedSelectorPattern.MatchString(selector) {
			return errors.New("invalid protected environment binding")
		}
		if envHasKey(*env, name) {
			return errSecretEnvCollision
		}
	}
	relative := strings.TrimPrefix(guestSecretProxyRoot, "/")
	if err := mkdirAllNoSymlink(imageRoot, relative, 0755); err != nil {
		return err
	}
	directory, err := confinedLayerPath(imageRoot, relative)
	if err != nil {
		return err
	}
	// Use the guest base's public system roots as well, preserving ordinary HTTPS.
	roots, err := os.ReadFile("/etc/ssl/certs/ca-certificates.crt")
	if err != nil {
		return errors.New("workspace Secret transport requires guest system CA bundle")
	}
	bundle := append(append(roots, '\n'), ca...)
	if err := writeFileNoFollow(filepath.Join(directory, "ca.pem"), ca, 0444); err != nil {
		return err
	}
	if err := writeFileNoFollow(filepath.Join(directory, "bundle.pem"), bundle, 0444); err != nil {
		return err
	}
	for name, selector := range selectors {
		*env = setEnvValue(*env, name, selector)
	}
	// Replace proxy routing variables together; a user NO_PROXY must not silently
	// bypass the transport for a supported client's protected request.
	filtered := (*env)[:0]
	for _, entry := range *env {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			continue
		}
		filtered = append(filtered, entry)
	}
	*env = filtered
	for name, value := range map[string]string{
		"HTTP_PROXY": "http://192.168.127.1:3128", "HTTPS_PROXY": "http://192.168.127.1:3128",
		"http_proxy": "http://192.168.127.1:3128", "https_proxy": "http://192.168.127.1:3128",
		"NO_PROXY": "", "no_proxy": "", "SSL_CERT_FILE": guestSecretProxyRoot + "/bundle.pem",
		"NODE_EXTRA_CA_CERTS": guestSecretProxyRoot + "/ca.pem", "NODE_USE_ENV_PROXY": "1",
	} {
		*env = setEnvValue(*env, name, value)
	}
	return nil
}
