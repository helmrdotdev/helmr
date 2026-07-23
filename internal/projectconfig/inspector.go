package projectconfig

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

//go:embed js/inspect.js js/register.mjs js/loader.mjs js/manifest.json
var inspectorFS embed.FS

var inspectorFiles = []string{
	"inspect.js",
	"register.mjs",
	"loader.mjs",
	"manifest.json",
}

type Inspector struct {
	Dir          string
	ScriptPath   string
	RegisterPath string
}

func Ensure() (Inspector, error) {
	files, digest, err := embeddedFiles()
	if err != nil {
		return Inspector{}, err
	}
	root, err := cacheRoot()
	if err != nil {
		return Inspector{}, err
	}
	if err := ensurePrivateDir(root); err != nil {
		return Inspector{}, fmt.Errorf("prepare config inspector cache: %w", err)
	}
	target := filepath.Join(root, digest)
	if err := verifyDir(target, files); err == nil {
		return paths(target), nil
	}
	unlock, err := lockCache(root)
	if err != nil {
		return Inspector{}, err
	}
	defer unlock()
	if err := verifyDir(target, files); err == nil {
		return paths(target), nil
	}
	tmp, err := os.MkdirTemp(root, ".tmp-"+digest+"-")
	if err != nil {
		return Inspector{}, fmt.Errorf("create config inspector cache temp dir: %w", err)
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := writeDir(tmp, files); err != nil {
		return Inspector{}, err
	}
	if err := verifyDir(tmp, files); err != nil {
		return Inspector{}, fmt.Errorf("verify staged config inspector: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		if verifyDir(target, files) == nil {
			return paths(target), nil
		}
		_ = os.RemoveAll(target)
		if renameErr := os.Rename(tmp, target); renameErr != nil {
			return Inspector{}, fmt.Errorf("publish config inspector cache: %w", renameErr)
		}
	}
	cleanupTmp = false
	return paths(target), nil
}

func embeddedFiles() (map[string][]byte, string, error) {
	files := make(map[string][]byte, len(inspectorFiles))
	for _, name := range inspectorFiles {
		body, err := inspectorFS.ReadFile(filepath.ToSlash(filepath.Join("js", name)))
		if err != nil {
			return nil, "", fmt.Errorf("read embedded config inspector %s: %w", name, err)
		}
		files[name] = body
	}
	names := append([]string(nil), inspectorFiles...)
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		hash.Write([]byte(name))
		hash.Write([]byte{0})
		hash.Write(files[name])
		hash.Write([]byte{0})
	}
	return files, "sha256-" + hex.EncodeToString(hash.Sum(nil)), nil
}

func cacheRoot() (string, error) {
	if explicit := os.Getenv("HELMR_CONFIG_CACHE_DIR"); explicit != "" {
		return filepath.Join(explicit, "config"), nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		dir, err := temporaryCacheRoot()
		if err != nil {
			return "", fmt.Errorf("create temporary config cache root: %w", err)
		}
		return filepath.Join(dir, "config"), nil
	}
	return filepath.Join(dir, "helmr", "config"), nil
}

func paths(dir string) Inspector {
	return Inspector{
		Dir:          dir,
		ScriptPath:   filepath.Join(dir, "inspect.js"),
		RegisterPath: filepath.Join(dir, "register.mjs"),
	}
}

func writeDir(dir string, files map[string][]byte) error {
	for _, name := range inspectorFiles {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, files[name], 0o644); err != nil {
			return fmt.Errorf("write embedded config inspector %s: %w", name, err)
		}
	}
	return nil
}

func verifyDir(dir string, files map[string][]byte) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("config cache path is not a directory: %s", dir)
	}
	for _, name := range inspectorFiles {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("config cache path is not a regular file: %s", path)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !equalBytes(body, files[name]) {
			return errors.New("config cache contents do not match embedded inspector")
		}
	}
	return nil
}

func equalBytes(a []byte, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
