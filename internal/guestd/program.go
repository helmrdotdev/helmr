package guestd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
)

const (
	managedProgramNode             = "/opt/helmr/runtime/bin/node"
	managedProgramEntry            = "/opt/helmr/runtime/helmr/entry.mjs"
	managedRuntimeMetadata         = "/var/lib/helmr/program/runtime/helmr/runtime.json"
	maxProgramSecretPlacements     = 64
	maxProgramSecretPlaintextBytes = 128 << 20
	maxProgramSecretFrameBytes     = maxProgramSecretPlaintextBytes + 64<<10
	maxProgramControlFrameBytes    = 64 << 10
)

type programConnection interface {
	io.ReadWriteCloser
	SetReadDeadline(deadline time.Time) error
	SetWriteDeadline(deadline time.Time) error
}

func validateProgramSecrets(secrets []*agentv1.SessionSecret) error {
	envNames := make(map[string]struct{}, len(secrets))
	previousPlacement := ""
	previousFile := ""
	for _, secret := range secrets {
		if secret == nil {
			return errors.New("program secret is required")
		}
		switch placement := secret.GetPlacement().(type) {
		case *agentv1.SessionSecret_Env:
			name := strings.TrimSpace(placement.Env)
			if placement.Env != name || !validEnvironmentName(name) {
				return errors.New("program secret environment placement is invalid")
			}
			if reservedSecretEnv(name) {
				return fmt.Errorf(
					"program secret environment placement %q is reserved",
					name,
				)
			}
			if _, exists := envNames[name]; exists {
				return fmt.Errorf(
					"program secret environment placement %q is duplicated",
					name,
				)
			}
			if strings.IndexByte(string(secret.GetValue()), 0) >= 0 {
				return fmt.Errorf(
					"program secret environment placement %q contains NUL",
					name,
				)
			}
			envNames[name] = struct{}{}
			key := "env\x00" + name
			if previousPlacement >= key {
				return errors.New("program secret deliveries are not in canonical order")
			}
			previousPlacement = key
		case *agentv1.SessionSecret_File:
			if err := validateProgramSecretFilePath(placement.File); err != nil {
				return err
			}
			key := "file\x00" + placement.File
			if previousPlacement >= key {
				return errors.New("program secret deliveries are not in canonical order")
			}
			if previousFile != "" &&
				strings.HasPrefix(placement.File, previousFile+"/") {
				return errors.New("program secret file placements conflict")
			}
			previousPlacement = key
			previousFile = placement.File
		default:
			return errors.New("program secret placement is required")
		}
	}
	return nil
}

func validEnvironmentName(value string) bool {
	if value == "" {
		return false
	}
	for index, char := range value {
		if (char >= 'A' && char <= 'Z') ||
			(char >= 'a' && char <= 'z') ||
			char == '_' ||
			(index > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}

func validateProgramSecretFilePath(value string) error {
	if value == "" ||
		value != strings.TrimSpace(value) ||
		!strings.HasPrefix(value, "/") ||
		strings.ContainsRune(value, 0) ||
		path.Clean(value) != value ||
		value == "/" {
		return errors.New("program secret file placement is invalid")
	}
	if value == "/run/helmr" || strings.HasPrefix(value, "/run/helmr/") || value == "/workspace" ||
		strings.HasPrefix(value, "/workspace/") ||
		value == "/var/lib/helmr" ||
		strings.HasPrefix(value, "/var/lib/helmr/") ||
		isReservedRuntimePath(value) {
		return fmt.Errorf(
			"program secret file placement %q conflicts with reserved runtime paths",
			value,
		)
	}
	return nil
}

func managedProgramNodeFlags(runtimeRoot string) ([]string, error) {
	file, err := os.Open(filepath.Join(runtimeRoot, "helmr/runtime.json"))
	if err != nil {
		return nil, fmt.Errorf("open managed Runtime metadata: %w", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(
		file,
		(1<<20)+1,
	))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	metadata, err := artifact.ParseRuntimeMetadata(raw)
	if err != nil {
		return nil, fmt.Errorf("parse managed Runtime metadata: %w", err)
	}
	return append([]string(nil), metadata.ProgramNodeFlags...), nil
}

func stageProgramSecrets(
	imageRoot string,
	secrets []*agentv1.SessionSecret,
	runtimeUser *resolvedRuntimeUser,
	env *[]string,
) (string, func(), error) {
	defer clearProgramSecretValues(secrets)
	if err := validateProgramSecrets(secrets); err != nil {
		return "", func() {}, err
	}
	secretRoot, err := mkdirGuestdTemp("secrets-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(secretRoot) }
	var targetCleanups []func()
	cleanupAll := func() {
		for index := len(targetCleanups) - 1; index >= 0; index-- {
			targetCleanups[index]()
		}
		cleanup()
	}
	for _, secret := range secrets {
		switch placement := secret.GetPlacement().(type) {
		case *agentv1.SessionSecret_Env:
			if envHasKey(*env, placement.Env) {
				cleanupAll()
				return "", func() {}, errSecretEnvCollision
			}
			*env = setEnvValue(*env, placement.Env, string(secret.GetValue()))
		case *agentv1.SessionSecret_File:
			targetCleanup, err := acquireProgramSecretTarget(
				imageRoot,
				placement.File,
			)
			if err != nil {
				cleanupAll()
				return "", func() {}, err
			}
			targetCleanups = append(targetCleanups, targetCleanup)
			relative := strings.TrimPrefix(placement.File, "/")
			parent := filepath.Dir(relative)
			if parent != "." {
				if err := mkdirAllNoSymlink(
					secretRoot,
					filepath.ToSlash(parent),
					0o700,
				); err != nil {
					cleanupAll()
					return "", func() {}, err
				}
			}
			target, err := confinedLayerPath(
				secretRoot,
				relative,
			)
			if err != nil {
				cleanupAll()
				return "", func() {}, err
			}
			if err := writeFileNoFollow(target, secret.GetValue(), 0o400); err != nil {
				cleanupAll()
				return "", func() {}, err
			}
			if runtimeUser != nil && os.Geteuid() == 0 {
				if err := os.Chown(
					target,
					int(runtimeUser.UID),
					int(runtimeUser.GID),
				); err != nil {
					cleanupAll()
					return "", func() {}, err
				}
			}
		}
	}
	return secretRoot, cleanupAll, nil
}

func prepareProgramSecretTarget(
	imageRoot string,
	guestPath string,
) (func(), error) {
	relative := strings.TrimPrefix(guestPath, "/")
	parent := filepath.Dir(relative)
	var createdDirectories []string
	cleanupDirectories := func() {
		for _, directory := range createdDirectories {
			_ = os.Remove(directory)
		}
	}
	for current := parent; current != "." && current != ""; current = filepath.Dir(current) {
		hostPath, err := confinedLayerPath(imageRoot, current)
		if err != nil {
			cleanupDirectories()
			return func() {}, err
		}
		if _, err := os.Lstat(hostPath); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			cleanupDirectories()
			return func() {}, err
		}
		createdDirectories = append(createdDirectories, hostPath)
	}
	if parent != "." {
		if err := mkdirAllNoSymlink(
			imageRoot,
			filepath.ToSlash(parent),
			0o755,
		); err != nil {
			cleanupDirectories()
			return func() {}, err
		}
	}
	target, err := confinedLayerPath(imageRoot, relative)
	if err != nil {
		cleanupDirectories()
		return func() {}, err
	}
	info, err := os.Lstat(target)
	if err == nil {
		if !info.Mode().IsRegular() {
			cleanupDirectories()
			return func() {}, fmt.Errorf(
				"program secret target is not a regular file: %s",
				guestPath,
			)
		}
		return func() {}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		cleanupDirectories()
		return func() {}, err
	}
	file, err := os.OpenFile(
		target,
		os.O_CREATE|os.O_EXCL|os.O_WRONLY,
		0o000,
	)
	if err != nil {
		cleanupDirectories()
		return func() {}, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(target)
		cleanupDirectories()
		return func() {}, err
	}
	return func() {
		_ = os.Remove(target)
		cleanupDirectories()
	}, nil
}

func clearProgramSecretValues(secrets []*agentv1.SessionSecret) {
	for _, secret := range secrets {
		if secret == nil {
			continue
		}
		for index := range secret.Value {
			secret.Value[index] = 0
		}
		secret.Value = nil
	}
}

var programSecretTargets = struct {
	sync.Mutex
	entries map[string]*programSecretTarget
}{entries: make(map[string]*programSecretTarget)}

type programSecretTarget struct {
	references int
	cleanup    func()
}

func acquireProgramSecretTarget(imageRoot, guestPath string) (func(), error) {
	key := filepath.Join(imageRoot, strings.TrimPrefix(guestPath, "/"))
	programSecretTargets.Lock()
	defer programSecretTargets.Unlock()
	target := programSecretTargets.entries[key]
	if target == nil {
		cleanup, err := prepareProgramSecretTarget(imageRoot, guestPath)
		if err != nil {
			return func() {}, err
		}
		target = &programSecretTarget{cleanup: cleanup}
		programSecretTargets.entries[key] = target
	}
	target.references++
	return sync.OnceFunc(func() {
		programSecretTargets.Lock()
		defer programSecretTargets.Unlock()
		target.references--
		if target.references == 0 {
			target.cleanup()
			delete(programSecretTargets.entries, key)
		}
	}), nil
}
