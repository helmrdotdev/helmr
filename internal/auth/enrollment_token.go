package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// A worker group's enrollment token is reusable until rotated, for fleets that
// keep it in their own secret store. A single-use token admits one machine and
// expires. The prefix tells them apart.
const (
	EnrollmentTokenPrefix          = "hlmr_wgt_"
	SingleUseEnrollmentTokenPrefix = "hlmr_wet_"
	enrollmentTokenBytes           = 32
)

type GeneratedEnrollmentToken struct {
	Raw  string
	Hash []byte
}

// ParsedEnrollmentToken is the hash of a well-formed enrollment token and
// whether it is single-use.
type ParsedEnrollmentToken struct {
	Hash      []byte
	SingleUse bool
}

func GenerateEnrollmentToken() (GeneratedEnrollmentToken, error) {
	return generateEnrollmentToken(EnrollmentTokenPrefix)
}

func GenerateSingleUseEnrollmentToken() (GeneratedEnrollmentToken, error) {
	return generateEnrollmentToken(SingleUseEnrollmentTokenPrefix)
}

func generateEnrollmentToken(prefix string) (GeneratedEnrollmentToken, error) {
	opaque, err := GenerateOpaque(enrollmentTokenBytes)
	if err != nil {
		return GeneratedEnrollmentToken{}, fmt.Errorf("generate worker enrollment token: %w", err)
	}
	raw := prefix + opaque
	return GeneratedEnrollmentToken{Raw: raw, Hash: HashCredential(raw)}, nil
}

func ParseEnrollmentToken(raw string) (ParsedEnrollmentToken, error) {
	if raw == "" {
		return ParsedEnrollmentToken{}, errors.New("worker enrollment token is required")
	}
	if strings.TrimSpace(raw) != raw {
		return ParsedEnrollmentToken{}, errors.New("worker enrollment token is invalid")
	}
	var prefix string
	switch {
	case strings.HasPrefix(raw, EnrollmentTokenPrefix):
		prefix = EnrollmentTokenPrefix
	case strings.HasPrefix(raw, SingleUseEnrollmentTokenPrefix):
		prefix = SingleUseEnrollmentTokenPrefix
	default:
		return ParsedEnrollmentToken{}, errors.New("worker enrollment token is invalid")
	}
	encoded := raw[len(prefix):]
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != enrollmentTokenBytes || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return ParsedEnrollmentToken{}, errors.New("worker enrollment token is invalid")
	}
	return ParsedEnrollmentToken{Hash: HashCredential(raw), SingleUse: prefix == SingleUseEnrollmentTokenPrefix}, nil
}
