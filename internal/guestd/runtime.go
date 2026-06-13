package guestd

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/safepath"
)

const (
	defaultRuntimeWorkdir = "/workspace"
	defaultRuntimePath    = "/usr/local/bin:/usr/bin:/bin"
)

func installAdapterBundle(adapterBundlePath, imageRoot string) error {
	if strings.TrimSpace(adapterBundlePath) == "" {
		return errors.New("adapter bundle path is required")
	}
	if err := mkdirAllNoSymlink(imageRoot, "opt", 0o755); err != nil {
		return err
	}
	target, err := safepath.JoinSlash(imageRoot, "opt/helmr")
	if err != nil {
		return err
	}
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		return err
	}
	return copyTreeSkipping(adapterBundlePath, target, nil)
}

func materializeDeploymentSourceForRuntime(imageRoot string, sourceRoot string, launchCwd string, runtimeUser *resolvedRuntimeUser) (string, error) {
	runtimePath := path.Join(launchCwd, ".helmr", "deployment-source")
	if isReservedRuntimePath(runtimePath) {
		return "", fmt.Errorf("deployment source path %s conflicts with reserved runtime paths", runtimePath)
	}
	parent := path.Join(strings.TrimPrefix(runtimePath, "/"), "..")
	if err := mkdirAllNoSymlink(imageRoot, parent, 0o755); err != nil {
		return "", err
	}
	target, err := safepath.JoinSlash(imageRoot, strings.TrimPrefix(runtimePath, "/"))
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(target); err != nil {
		return "", err
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		return "", err
	}
	if err := copyTreeSkipping(sourceRoot, target, isDeploymentSourceRuntimeExcluded); err != nil {
		return "", fmt.Errorf("materialize deployment source: %w", err)
	}
	if runtimeUser != nil {
		if err := chownTree(target, runtimeUser.UID, runtimeUser.GID); err != nil {
			return "", fmt.Errorf("prepare deployment source owner: %w", err)
		}
	}
	return runtimePath, nil
}

func isDeploymentSourceRuntimeExcluded(rel string, isDir bool) bool {
	parts := strings.SplitSeq(filepath.ToSlash(rel), "/")
	for part := range parts {
		if part == "node_modules" {
			return true
		}
	}
	return false
}

func imageNodeRuntimeCommand(imageRoot string, imageConfig ociRuntimeConfig) (string, error) {
	pathValue := defaultRuntimePath
	for _, entry := range sanitizeDynamicLoaderEnv(imageConfig.Env) {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == "PATH" && strings.TrimSpace(value) != "" {
			pathValue = value
		}
	}
	for dir := range strings.SplitSeq(pathValue, ":") {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if !strings.HasPrefix(dir, "/") {
			continue
		}
		runtimePath := path.Clean(path.Join(dir, "node"))
		if isReservedRuntimePath(runtimePath) {
			continue
		}
		hostPath, err := safepath.JoinSlash(imageRoot, strings.TrimPrefix(runtimePath, "/"))
		if err != nil {
			return "", err
		}
		if isExecutableFile(hostPath) {
			return runtimePath, nil
		}
	}
	return "", errors.New("task image must provide an executable node in PATH for Helmr TypeScript tasks")
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func mergeEnv(groups ...[]string) []string {
	values := make(map[string]string)
	order := []string{}
	for _, group := range groups {
		for _, entry := range group {
			key, value, ok := strings.Cut(entry, "=")
			if !ok {
				continue
			}
			if _, exists := values[key]; !exists {
				order = append(order, key)
			}
			values[key] = value
		}
	}
	env := make([]string, 0, len(order))
	for _, key := range order {
		env = append(env, key+"="+values[key])
	}
	return env
}

func imageRuntimeEnv(imageConfig ociRuntimeConfig, runtimeUser *resolvedRuntimeUser, launchCwd string) []string {
	env := mergeEnv(sanitizeDynamicLoaderEnv(imageConfig.Env), nil)
	env = setEnvDefault(env, "PATH", defaultRuntimePath)
	env = setEnvDefault(env, "HOME", runtimeUser.Home)
	env = setEnvDefault(env, "USER", runtimeUser.Name)
	env = setEnvDefault(env, "LOGNAME", runtimeUser.Name)
	env = setEnvValue(env, "PWD", launchCwd)
	return env
}

func sanitizeDynamicLoaderEnv(env []string) []string {
	sanitized := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || isDynamicLoaderEnvKey(key) {
			continue
		}
		sanitized = append(sanitized, entry)
	}
	return sanitized
}

func isDynamicLoaderEnvKey(key string) bool {
	return strings.HasPrefix(key, "LD_")
}

func setEnvDefault(env []string, key string, value string) []string {
	if envHasKey(env, key) {
		return env
	}
	return append(env, key+"="+value)
}

func setEnvValue(env []string, key string, value string) []string {
	for i, entry := range env {
		entryKey, _, ok := strings.Cut(entry, "=")
		if ok && entryKey == key {
			env[i] = key + "=" + value
			return env
		}
	}
	return append(env, key+"="+value)
}

func envHasKey(env []string, key string) bool {
	for _, entry := range env {
		entryKey, _, ok := strings.Cut(entry, "=")
		if ok && entryKey == key {
			return true
		}
	}
	return false
}
