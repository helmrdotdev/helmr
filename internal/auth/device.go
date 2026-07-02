package auth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/token"
)

type DeviceCodes struct {
	DeviceCode string
	UserCode   string
}

const (
	deviceCodeBytes = 32
	userCodeChars   = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

func GenerateDeviceCodes() (DeviceCodes, error) {
	deviceCode, err := token.GenerateOpaque(deviceCodeBytes)
	if err != nil {
		return DeviceCodes{}, err
	}
	userCode, err := generateUserCode()
	if err != nil {
		return DeviceCodes{}, err
	}
	return DeviceCodes{DeviceCode: deviceCode, UserCode: userCode}, nil
}

func NormalizeUserCode(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, " ", "")
	value = strings.ReplaceAll(value, "-", "")
	if len(value) <= 4 {
		return value
	}
	return value[:4] + "-" + value[4:]
}

func generateUserCode() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate user code: %w", err)
	}
	if len(userCodeChars) == 0 {
		return "", errors.New("user code alphabet is empty")
	}
	var builder strings.Builder
	for i, value := range raw {
		if i == 4 {
			builder.WriteByte('-')
		}
		builder.WriteByte(userCodeChars[int(value)%len(userCodeChars)])
	}
	return builder.String(), nil
}
