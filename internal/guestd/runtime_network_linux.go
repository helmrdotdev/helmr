//go:build linux

package guestd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/safepath"
)

const (
	imageRuntimeResolverPath = "/run/resolv.conf"
	imageRuntimeHostname     = "helmr-sandbox"
)

func setupImageRuntimeNetworkFiles(imageRoot string) error {
	resolver, err := os.ReadFile(imageRuntimeResolverPath)
	if err != nil {
		return fmt.Errorf("read runtime resolver contract: %w", err)
	}
	resolver = normalizeRuntimeResolver(resolver)
	if len(resolver) == 0 {
		return errors.New("runtime resolver contract is empty")
	}
	if err := writeImageRuntimeFile(imageRoot, "etc/resolv.conf", resolver, 0o644); err != nil {
		return err
	}
	if err := writeImageRuntimeFile(imageRoot, "etc/hostname", []byte(imageRuntimeHostname+"\n"), 0o644); err != nil {
		return err
	}
	return writeImageRuntimeFile(imageRoot, "etc/hosts", imageRuntimeHostsFile(imageRuntimeHostname), 0o644)
}

func normalizeRuntimeResolver(raw []byte) []byte {
	var output bytes.Buffer
	for _, line := range bytes.Split(raw, []byte("\n")) {
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		output.WriteString(trimmed)
		output.WriteByte('\n')
	}
	return output.Bytes()
}

func imageRuntimeHostsFile(hostname string) []byte {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		hostname = imageRuntimeHostname
	}
	return []byte(strings.Join([]string{
		"127.0.0.1 localhost " + hostname,
		"::1 localhost ip6-localhost ip6-loopback",
		"",
	}, "\n"))
}

func writeImageRuntimeFile(imageRoot string, rel string, contents []byte, mode os.FileMode) error {
	parent := filepath.ToSlash(filepath.Dir(rel))
	if parent == "." {
		parent = ""
	}
	if err := mkdirAllNoSymlink(imageRoot, parent, 0o755); err != nil {
		return err
	}
	target, err := safepath.JoinSlash(imageRoot, filepath.ToSlash(rel))
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove image runtime file %s: %w", rel, err)
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return fmt.Errorf("create image runtime file %s: %w", rel, err)
	}
	_, writeErr := file.Write(contents)
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write image runtime file %s: %w", rel, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close image runtime file %s: %w", rel, closeErr)
	}
	return nil
}
