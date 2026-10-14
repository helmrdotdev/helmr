package guestd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

const programCgroupLeafPrefix = "run-"

func execCgroupLeafName(commandID string) (string, error) {
	if strings.TrimSpace(commandID) == "" {
		return "", errors.New("exec cgroup identity is required")
	}
	sum := sha256.Sum256([]byte("exec\x00" + commandID))
	return "exec-" + hex.EncodeToString(sum[:16]), nil
}

func sessionCgroupLeafName(sessionID string, processEpoch int64) (string, error) {
	if strings.TrimSpace(sessionID) == "" || strings.ContainsRune(sessionID, '\x00') || processEpoch <= 0 {
		return "", errors.New("session process identity is incomplete")
	}
	sum := sha256.Sum256([]byte("session\x00" + sessionID + "\x00" + strconv.FormatInt(processEpoch, 10)))
	return "session-" + hex.EncodeToString(sum[:16]), nil
}

func validateProcessCgroupLeaf(leaf string) error {
	_, digest, found := strings.Cut(leaf, "-")
	if (!strings.HasPrefix(leaf, programCgroupLeafPrefix) && !strings.HasPrefix(leaf, "exec-") && !strings.HasPrefix(leaf, "session-")) || !found || len(digest) != 32 {
		return errors.New("process cgroup leaf is invalid")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return errors.New("process cgroup leaf is invalid")
	}
	return nil
}

type processCgroup interface {
	attach(*exec.Cmd) error
	freeze(context.Context) error
	thaw(context.Context) error
	kill() error
	waitEmpty() error
	close() error
}
