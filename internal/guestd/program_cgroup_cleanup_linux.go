//go:build linux

package guestd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	programCgroupRoot           = "/sys/fs/cgroup/helmr"
	programCgroupCleanupTimeout = 10 * time.Second
)

func killCgroup(path string) error {
	if err := os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0o644); err != nil {
		return fmt.Errorf("kill cgroup: %w", err)
	}
	return nil
}

func waitCgroupEmpty(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), programCgroupCleanupTimeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		raw, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
		if err != nil {
			return fmt.Errorf("read cgroup events: %w", err)
		}
		populated, err := parseCgroupPopulated(raw)
		if err != nil {
			return err
		}
		if !populated {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for cgroup to empty: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func parseCgroupPopulated(raw []byte) (bool, error) {
	found := false
	populated := false
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		fields := bytes.Fields(line)
		if len(fields) != 2 || !bytes.Equal(fields[0], []byte("populated")) {
			continue
		}
		if found {
			return false, errors.New("cgroup events repeats populated")
		}
		found = true
		switch string(fields[1]) {
		case "0":
			populated = false
		case "1":
			populated = true
		default:
			return false, fmt.Errorf("cgroup populated = %q", fields[1])
		}
	}
	if !found {
		return false, errors.New("cgroup events omits populated")
	}
	return populated, nil
}
