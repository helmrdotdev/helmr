package computerhost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CheckComputerAttachmentsReleased refuses a successful recovery report while
// attachment custody needs explicit reconciliation. It never releases custody.
func CheckComputerAttachmentsReleased(tempDir string, devices []string) error {
	if err := checkComputerAttachmentResidue(tempDir); err != nil {
		return err
	}
	return checkComputerDevicesIdle(devices)
}

func checkComputerAttachmentResidue(root string) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("computer attachment recovery requires a private temporary directory")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect computer temporary directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("computer temporary directory %s is not a direct directory", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("inventory computer attachments: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "computer-") {
			continue
		}
		arena := filepath.Join(root, entry.Name())
		info, err := os.Lstat(arena)
		if err != nil {
			return fmt.Errorf("inspect computer arena %s: %w", arena, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("computer arena %s requires reconciliation: not a direct directory", arena)
		}
		// Reading the directory also rejects unreadable arenas even when no known
		// marker was observed. Marker contents never grant cleanup authority.
		if _, err := os.ReadDir(arena); err != nil {
			return fmt.Errorf("inventory computer arena %s: %w", arena, err)
		}
		for _, marker := range []string{"config.json", "claim.json", "nbd.sock"} {
			if _, err := os.Lstat(filepath.Join(arena, marker)); err == nil {
				return fmt.Errorf("computer arena %s requires attachment reconciliation: retained %s", arena, marker)
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("inspect computer attachment marker: %w", err)
			}
		}
	}
	return nil
}
