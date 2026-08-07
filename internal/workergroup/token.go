package workergroup

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/auth"
)

const (
	EnrollmentTokenPrefix = "hlmr_wgt_"
	enrollmentTokenBytes  = 32
)

type GeneratedEnrollmentToken struct {
	Raw  string
	Hash []byte
}

func GenerateEnrollmentToken() (GeneratedEnrollmentToken, error) {
	opaque, err := auth.GenerateOpaque(enrollmentTokenBytes)
	if err != nil {
		return GeneratedEnrollmentToken{}, fmt.Errorf("generate worker group token: %w", err)
	}
	raw := EnrollmentTokenPrefix + opaque
	return GeneratedEnrollmentToken{Raw: raw, Hash: auth.HashCredential(raw)}, nil
}

func ParseEnrollmentToken(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("worker enrollment token is required")
	}
	if strings.TrimSpace(raw) != raw || !strings.HasPrefix(raw, EnrollmentTokenPrefix) {
		return nil, errors.New("worker enrollment token is invalid")
	}
	encoded := raw[len(EnrollmentTokenPrefix):]
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != enrollmentTokenBytes || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return nil, errors.New("worker enrollment token is invalid")
	}
	return auth.HashCredential(raw), nil
}
