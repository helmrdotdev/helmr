package guestd

import (
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	defaultRuntimeWorkdir = "/workspace"
	defaultRuntimePath    = "/usr/local/bin:/usr/bin:/bin"
)

func installRuntimeBundle(runtimePath, imageRoot string) error {
	if strings.TrimSpace(runtimePath) == "" {
		return errors.New("runtime path is required")
	}
	if err := mkdirAllNoSymlink(imageRoot, "opt", 0o755); err != nil {
		return err
	}
	target, err := safeJoin(imageRoot, "opt/helmr")
	if err != nil {
		return err
	}
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		return err
	}
	return copyTree(runtimePath, target)
}

func materializeTaskSourceForRuntime(imageRoot string, sourceRoot string, launchCwd string, runtimeUser *resolvedRuntimeUser) (string, error) {
	runtimePath := pathpkg.Join(launchCwd, ".helmr", "task-source")
	if isReservedRuntimePath(runtimePath) {
		return "", fmt.Errorf("task source path %s conflicts with reserved runtime paths", runtimePath)
	}
	parent := pathpkg.Join(strings.TrimPrefix(runtimePath, "/"), "..")
	if err := mkdirAllNoSymlink(imageRoot, parent, 0o755); err != nil {
		return "", err
	}
	target, err := safeJoin(imageRoot, strings.TrimPrefix(runtimePath, "/"))
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(target); err != nil {
		return "", err
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		return "", err
	}
	if err := copyTree(sourceRoot, target); err != nil {
		return "", fmt.Errorf("materialize task source: %w", err)
	}
	if runtimeUser != nil {
		if err := chownTree(target, runtimeUser.UID, runtimeUser.GID); err != nil {
			return "", fmt.Errorf("prepare task source owner: %w", err)
		}
	}
	return runtimePath, nil
}

func bundledRuntimeCommand(imageRoot string) (string, []string, error) {
	bunHostPath, err := safeJoin(imageRoot, "opt/helmr/bin/bun")
	if err != nil {
		return "", nil, err
	}
	if !isExecutableFile(bunHostPath) {
		return "", nil, errors.New("runtime bundle must provide executable /opt/helmr/bin/bun")
	}
	libHostPath, err := safeJoin(imageRoot, "opt/helmr/lib")
	if err != nil {
		return "", nil, err
	}
	loaderName, err := findBundledRuntimeLoader(libHostPath)
	if err != nil {
		return "", nil, err
	}
	loaderPath := pathpkg.Join("/opt/helmr/lib", loaderName)
	return loaderPath, []string{"--library-path", "/opt/helmr/lib", "/opt/helmr/bin/bun"}, nil
}

func findBundledRuntimeLoader(libHostPath string) (string, error) {
	for _, name := range []string{"ld-linux-x86-64.so.2", "ld-linux-aarch64.so.1"} {
		if isExecutableFile(filepath.Join(libHostPath, name)) {
			return name, nil
		}
	}
	entries, err := os.ReadDir(libHostPath)
	if err != nil {
		return "", fmt.Errorf("read runtime bundle lib directory: %w", err)
	}
	var muslLoaders []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "ld-musl-") && strings.HasSuffix(name, ".so.1") {
			muslLoaders = append(muslLoaders, name)
		}
	}
	sort.Strings(muslLoaders)
	for _, name := range muslLoaders {
		if isExecutableFile(filepath.Join(libHostPath, name)) {
			return name, nil
		}
	}
	return "", errors.New("runtime bundle must provide an executable dynamic loader in /opt/helmr/lib")
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
	env := mergeEnv(sanitizeDynamicLoaderEnv(imageConfig.Env), []string{"HELMR_ADAPTER_SDK_PATH=/opt/helmr/adapter/sdk.js"})
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
