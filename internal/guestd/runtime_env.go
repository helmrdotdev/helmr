package guestd

import (
	"strings"
)

const (
	defaultRuntimeWorkdir = "/workspace"
	defaultRuntimePath    = "/usr/local/bin:/usr/bin:/bin"
)

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

func managedRuntimeEnv(imageConfig ociRuntimeConfig, runtimeUser *resolvedRuntimeUser, launchCwd string) []string {
	return sanitizeManagedRuntimeEnv(imageRuntimeEnv(imageConfig, runtimeUser, launchCwd))
}

func sanitizeManagedRuntimeEnv(env []string) []string {
	sanitized := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || isManagedRuntimeEnvKey(key) {
			continue
		}
		sanitized = append(sanitized, entry)
	}
	return sanitized
}

func isManagedRuntimeEnvKey(key string) bool {
	if isDynamicLoaderEnvKey(key) {
		return true
	}
	switch key {
	case "NODE_OPTIONS",
		"NODE_PATH",
		"NODE_EXTRA_CA_CERTS",
		"NODE_ICU_DATA",
		"SSL_CERT_FILE",
		"SSL_CERT_DIR",
		"OPENSSL_CONF",
		"OPENSSL_MODULES",
		"OPENSSL_ENGINES",
		"GCONV_PATH",
		"LOCPATH":
		return true
	default:
		return false
	}
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
