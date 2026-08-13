package builder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type InstallPlan struct {
	Argv          []string
	CustomCommand string
}

// SelectInstallPlan is producer convenience, not deployment admission. It
// respects the project's top-level packageManager selector or an unambiguous
// lockfile without putting either identity into the final bundle. An explicit
// custom command bypasses package-manager inference entirely.
func SelectInstallPlan(project, customCommand string) (InstallPlan, error) {
	customCommand = strings.TrimSpace(customCommand)
	if customCommand != "" {
		return InstallPlan{CustomCommand: customCommand}, nil
	}
	if project == "" || !filepath.IsAbs(project) || filepath.Clean(project) != project {
		return InstallPlan{}, errors.New("install project must be an absolute clean path")
	}
	raw, err := os.ReadFile(filepath.Join(project, "package.json"))
	if err != nil {
		return InstallPlan{}, fmt.Errorf("read package.json: %w", err)
	}
	var manifest struct {
		PackageManager string `json:"packageManager"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&manifest); err != nil {
		return InstallPlan{}, fmt.Errorf("decode package.json: %w", err)
	}
	if err := ensureInstallJSONEOF(decoder); err != nil {
		return InstallPlan{}, err
	}
	manager, selector, err := selectedManager(project, strings.TrimSpace(manifest.PackageManager))
	if err != nil {
		return InstallPlan{}, err
	}
	return installArgv(project, manager, selector), nil
}

func selectedManager(project, selector string) (string, string, error) {
	if selector != "" {
		for _, manager := range []string{"npm", "pnpm", "bun", "yarn"} {
			prefix := manager + "@"
			if !strings.HasPrefix(selector, prefix) {
				continue
			}
			version := strings.TrimSpace(strings.TrimPrefix(selector, prefix))
			if version == "" || len(version) > 512 || strings.ContainsAny(version, "\x00\r\n") {
				return "", "", errors.New("packageManager selector version is invalid")
			}
			return manager, manager + "@" + version, nil
		}
		return "", "", errors.New("packageManager is not available in the canonical builder; use --install-command")
	}

	families := make(map[string]struct{})
	for manager, names := range map[string][]string{
		"npm":  {"package-lock.json", "npm-shrinkwrap.json"},
		"pnpm": {"pnpm-lock.yaml"},
		"bun":  {"bun.lock", "bun.lockb"},
		"yarn": {"yarn.lock"},
	} {
		for _, name := range names {
			if info, err := os.Lstat(filepath.Join(project, name)); err == nil && info.Mode().IsRegular() {
				families[manager] = struct{}{}
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", "", fmt.Errorf("inspect %s: %w", name, err)
			}
		}
	}
	if len(families) > 1 {
		names := make([]string, 0, len(families))
		for name := range families {
			names = append(names, name)
		}
		sort.Strings(names)
		return "", "", fmt.Errorf("package-manager lockfiles are ambiguous: %s", strings.Join(names, ", "))
	}
	for name := range families {
		return name, "", nil
	}
	return "npm", "", nil
}

func installArgv(project, manager, selector string) InstallPlan {
	executable := manager
	arguments := []string{}
	if selector != "" {
		switch manager {
		case "npm", "pnpm", "yarn":
			executable = "corepack"
			arguments = append(arguments, selector)
		default:
			executable = "npx"
			arguments = append(arguments, "--yes", bunPackageSelector(selector))
		}
	}
	switch manager {
	case "npm":
		if regularInstallFile(project, "package-lock.json") || regularInstallFile(project, "npm-shrinkwrap.json") {
			arguments = append(arguments, "ci")
		} else {
			arguments = append(arguments, "install")
		}
		arguments = append(arguments, "--no-audit", "--no-fund")
	case "pnpm":
		arguments = append(arguments, "install")
		if regularInstallFile(project, "pnpm-lock.yaml") {
			arguments = append(arguments, "--frozen-lockfile")
		}
	case "bun":
		arguments = append(arguments, "install")
		if regularInstallFile(project, "bun.lock") || regularInstallFile(project, "bun.lockb") {
			arguments = append(arguments, "--frozen-lockfile")
		}
	case "yarn":
		arguments = append(arguments, "install")
		if regularInstallFile(project, "yarn.lock") {
			arguments = append(arguments, "--immutable")
		}
	}
	return InstallPlan{Argv: append([]string{executable}, arguments...)}
}

func bunPackageSelector(selector string) string {
	for _, algorithm := range []string{"+sha224.", "+sha256.", "+sha384.", "+sha512."} {
		index := strings.LastIndex(selector, algorithm)
		if index < 0 {
			continue
		}
		digest := selector[index+len(algorithm):]
		if digest != "" && strings.IndexFunc(digest, func(character rune) bool {
			return character < '0' || character > '9' && character < 'a' || character > 'f'
		}) == -1 {
			return selector[:index]
		}
	}
	return selector
}

func regularInstallFile(project, name string) bool {
	info, err := os.Lstat(filepath.Join(project, name))
	return err == nil && info.Mode().IsRegular()
}

func ensureInstallJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("package.json contains trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode package.json trailing data: %w", err)
	}
	return nil
}
