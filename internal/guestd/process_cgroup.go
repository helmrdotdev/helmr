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

func programCgroupLeafName(runID string, attemptNumber uint32, runLeaseID string) (string, error) {
	runID = strings.TrimSpace(runID)
	runLeaseID = strings.TrimSpace(runLeaseID)
	if runID == "" || attemptNumber == 0 || runLeaseID == "" {
		return "", errors.New("program cgroup identity is incomplete")
	}
	sum := sha256.Sum256([]byte(runID + "\x00" + strconv.FormatUint(uint64(attemptNumber), 10) + "\x00" + runLeaseID))
	return programCgroupLeafPrefix + hex.EncodeToString(sum[:16]), nil
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

// Only the guest constructs a nested assignment. Top-level creation continues
// to require a single validated leaf; no caller supplies a filesystem path.
func validateAssignedProcessCgroup(scope string) error {
	parent, child, nested := strings.Cut(scope, "/")
	if err := validateProcessCgroupLeaf(parent); err != nil {
		return err
	}
	if !nested {
		return nil
	}
	if !strings.HasPrefix(child, "native-") || len(child) != len("native-")+32 {
		return errors.New("native cgroup assignment is invalid")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(child, "native-")); err != nil {
		return errors.New("native cgroup assignment is invalid")
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
