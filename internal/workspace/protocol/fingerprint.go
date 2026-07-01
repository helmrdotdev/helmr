package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/stablejson"
)

const (
	GuestVerbStartExec = "StartExec"
	GuestVerbCreatePty = "CreatePty"
	GuestVerbResizePty = "ResizePty"
	GuestVerbClosePty  = "ClosePty"
)

func GuestVerb(operationKind string) (string, error) {
	switch strings.TrimSpace(operationKind) {
	case "start_exec":
		return GuestVerbStartExec, nil
	case "create_pty":
		return GuestVerbCreatePty, nil
	case "resize_pty":
		return GuestVerbResizePty, nil
	case "close_pty":
		return GuestVerbClosePty, nil
	default:
		return "", fmt.Errorf("unsupported workspace operation kind %q", strings.TrimSpace(operationKind))
	}
}

func RequestFingerprint(operationKind string, requestJSON []byte) (string, error) {
	stable, err := stablejson.Encode(requestJSON)
	if err != nil {
		return "", err
	}
	return requestFingerprint(operationKind, string(stable)), nil
}

func requestFingerprint(operationKind string, requestJSON string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(strings.TrimSpace(operationKind)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strings.TrimSpace(requestJSON)))
	return hex.EncodeToString(hash.Sum(nil))
}
