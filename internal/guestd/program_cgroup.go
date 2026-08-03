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

func programCgroupLeafName(runID string, attemptNumber uint32, runLeaseID string) (string, error) {
	runID = strings.TrimSpace(runID)
	runLeaseID = strings.TrimSpace(runLeaseID)
	if runID == "" || attemptNumber == 0 || runLeaseID == "" {
		return "", errors.New("program cgroup identity is incomplete")
	}
	sum := sha256.Sum256([]byte(runID + "\x00" + strconv.FormatUint(uint64(attemptNumber), 10) + "\x00" + runLeaseID))
	return programCgroupLeafPrefix + hex.EncodeToString(sum[:16]), nil
}

func validateProgramCgroupLeaf(leaf string) error {
	if !strings.HasPrefix(leaf, programCgroupLeafPrefix) ||
		len(leaf) != len(programCgroupLeafPrefix)+32 {
		return errors.New("program cgroup leaf is invalid")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(leaf, programCgroupLeafPrefix)); err != nil {
		return errors.New("program cgroup leaf is invalid")
	}
	return nil
}

type programCgroup interface {
	attach(*exec.Cmd) error
	freeze(context.Context) error
	thaw(context.Context) error
	kill() error
	waitEmpty() error
	close() error
}
