package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"golang.org/x/sys/unix"
)

const workerHostSecretFileName = "worker-host-secret.json"

type workerHostSecretFile struct {
	WorkerHostID     string    `json:"worker_host_id"`
	WorkerHostSecret string    `json:"worker_host_secret"`
	CreatedAt        time.Time `json:"created_at"`
	WorkDir          string    `json:"work_dir"`
	JailerDir        string    `json:"jailer_dir"`
}

func resolveWorkerHostSecret(ctx context.Context, cfg config.Worker, workDir string) (workerHostSecretFile, error) {
	workDir, jailerDir, err := resolveWorkerRecoveryRoots(workDir, cfg.JailerChrootDir)
	if err != nil {
		return workerHostSecretFile{}, err
	}
	validate := func(secret workerHostSecretFile) error {
		if secret.WorkDir != workDir || secret.JailerDir != jailerDir {
			return errors.New("worker host secret recovery roots are missing or do not match configured roots")
		}
		return nil
	}
	path := workerHostSecretPath(workDir, cfg.WorkerHostSecretPath)
	if hostSecret, err := readWorkerHostSecret(path); err == nil {
		return hostSecret, validate(hostSecret)
	} else if !errors.Is(err, os.ErrNotExist) {
		return workerHostSecretFile{}, err
	}
	var hostSecret workerHostSecretFile
	if err := withWorkerHostSecretLock(path, func() error {
		if stored, err := readWorkerHostSecret(path); err == nil {
			hostSecret = stored
			return validate(stored)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		enrollmentToken, err := readWorkerEnrollmentToken(cfg.WorkerEnrollmentTokenFile)
		if err != nil {
			return err
		}
		controlPlaneClient, err := workerclient.New(cfg.ControlPlaneURL)
		if err != nil {
			return fmt.Errorf("configure worker enrollment client: %w", err)
		}
		registered, err := controlPlaneClient.EnrollWorker(ctx, enrollmentToken, workerapi.EnrollmentRequest{
			ResourceID: cfg.WorkerResourceID, PoolName: cfg.WorkerPoolName,
		})
		if err != nil {
			return fmt.Errorf("enroll worker: %w", err)
		}
		registered.WorkerHostID = strings.TrimSpace(registered.WorkerHostID)
		registered.WorkerPoolID = strings.TrimSpace(registered.WorkerPoolID)
		registered.WorkerHostSecret = strings.TrimSpace(registered.WorkerHostSecret)
		if registered.WorkerHostID == "" {
			return errors.New("worker enrollment response worker_host_id is empty")
		}
		if registered.WorkerHostSecret == "" {
			return errors.New("worker enrollment response secret is empty")
		}
		if _, err := ids.Parse(registered.WorkerPoolID); err != nil {
			return errors.New("worker enrollment response worker_pool_id is not a canonical UUIDv7")
		}
		hostSecret = workerHostSecretFile{
			WorkerHostID:     registered.WorkerHostID,
			WorkerHostSecret: registered.WorkerHostSecret,
			CreatedAt:        time.Now().UTC(),
			WorkDir:          workDir,
			JailerDir:        jailerDir,
		}
		if err := writeWorkerHostSecret(path, hostSecret); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return workerHostSecretFile{}, err
	}
	return hostSecret, nil
}

func resolveAuthenticatedWorkerHostSecret(
	ctx context.Context,
	cfg config.Worker,
	workDir string,
	authenticate func(workerHostSecretFile) error,
) (workerHostSecretFile, error) {
	hostSecret, err := resolveWorkerHostSecret(ctx, cfg, workDir)
	if err != nil {
		return workerHostSecretFile{}, err
	}
	if err := authenticate(hostSecret); err == nil {
		return hostSecret, nil
	} else if !httpclient.IsStatus(err, http.StatusUnauthorized) {
		return workerHostSecretFile{}, fmt.Errorf("authenticate worker host secret: %w", err)
	}
	path := workerHostSecretPath(workDir, cfg.WorkerHostSecretPath)
	if err := removeWorkerHostSecretIfMatch(path, hostSecret); err != nil {
		return workerHostSecretFile{}, err
	}
	hostSecret, err = resolveWorkerHostSecret(ctx, cfg, workDir)
	if err != nil {
		return workerHostSecretFile{}, fmt.Errorf("replace rejected worker host secret: %w", err)
	}
	if err := authenticate(hostSecret); err != nil {
		return workerHostSecretFile{}, fmt.Errorf("authenticate replaced worker host secret: %w", err)
	}
	return hostSecret, nil
}

func removeWorkerHostSecretIfMatch(path string, rejected workerHostSecretFile) error {
	return withWorkerHostSecretLock(path, func() error {
		current, err := readWorkerHostSecret(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.WorkerHostID != rejected.WorkerHostID || current.WorkerHostSecret != rejected.WorkerHostSecret {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove rejected worker host secret: %w", err)
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return fmt.Errorf("sync rejected worker host secret removal: %w", err)
		}
		return nil
	})
}

func resolveWorkerControlPlaneHostSecret(cfg config.WorkerControlPlane, workDir string) (workerHostSecretFile, error) {
	path := workerHostSecretPath(workDir, cfg.WorkerHostSecretPath)
	return readWorkerHostSecret(path)
}

func workerHostSecretPath(workDir string, configured string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}
	return filepath.Join(workDir, workerHostSecretFileName)
}

func readWorkerEnrollmentToken(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("WORKER_ENROLLMENT_TOKEN_FILE is required for worker enrollment")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("read WORKER_ENROLLMENT_TOKEN_FILE: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("WORKER_ENROLLMENT_TOKEN_FILE must be a regular file")
	}
	if permissions := info.Mode().Perm(); permissions != 0o400 && permissions != 0o600 {
		return "", errors.New("WORKER_ENROLLMENT_TOKEN_FILE must have mode 0400 or 0600")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("open WORKER_ENROLLMENT_TOKEN_FILE without following links: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect WORKER_ENROLLMENT_TOKEN_FILE: %w", err)
	}
	if !opened.Mode().IsRegular() || opened.Mode().Perm() != info.Mode().Perm() {
		return "", errors.New("WORKER_ENROLLMENT_TOKEN_FILE changed type or permissions while opening")
	}
	const maximumTokenFileBytes = int64(128)
	secretBytes, err := io.ReadAll(io.LimitReader(file, maximumTokenFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read WORKER_ENROLLMENT_TOKEN_FILE: %w", err)
	}
	if len(secretBytes) > int(maximumTokenFileBytes) {
		return "", errors.New("WORKER_ENROLLMENT_TOKEN_FILE is too large")
	}
	secret := string(secretBytes)
	if _, err := auth.ParseEnrollmentToken(secret); err != nil {
		return "", fmt.Errorf("WORKER_ENROLLMENT_TOKEN_FILE: %w", err)
	}
	return secret, nil
}

func readWorkerHostSecret(path string) (workerHostSecretFile, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return workerHostSecretFile{}, err
	}
	if !info.Mode().IsRegular() {
		return workerHostSecretFile{}, fmt.Errorf("worker host secret %s is not a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		return workerHostSecretFile{}, fmt.Errorf("worker host secret %s must have mode 0600", path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return workerHostSecretFile{}, fmt.Errorf("open worker host secret %s without following links: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return workerHostSecretFile{}, fmt.Errorf("inspect opened worker host secret %s: %w", path, err)
	}
	if !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 {
		return workerHostSecretFile{}, fmt.Errorf("opened worker host secret %s changed type or permissions", path)
	}
	bytes, err := io.ReadAll(file)
	if err != nil {
		return workerHostSecretFile{}, fmt.Errorf("read worker host secret %s: %w", path, err)
	}
	var hostSecret workerHostSecretFile
	if err := json.Unmarshal(bytes, &hostSecret); err != nil {
		return workerHostSecretFile{}, fmt.Errorf("read worker host secret %s: %w", path, err)
	}
	hostSecret.WorkerHostID = strings.TrimSpace(hostSecret.WorkerHostID)
	hostSecret.WorkerHostSecret = strings.TrimSpace(hostSecret.WorkerHostSecret)
	if hostSecret.WorkerHostID == "" || hostSecret.WorkerHostSecret == "" {
		return workerHostSecretFile{}, fmt.Errorf("worker host secret %s is incomplete", path)
	}
	return hostSecret, nil
}

func writeWorkerHostSecret(path string, hostSecret workerHostSecretFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create worker host secret directory: %w", err)
	}
	bytes, err := json.MarshalIndent(hostSecret, "", "  ")
	if err != nil {
		return fmt.Errorf("encode worker host secret: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create worker host secret temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(append(bytes, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write worker host secret temp file: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod worker host secret temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync worker host secret temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close worker host secret temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install worker host secret file: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync worker host secret directory: %w", err)
	}
	return nil
}

func withWorkerHostSecretLock(path string, fn func() error) error {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("create worker host secret lock directory: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open worker host secret lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock worker host secret: %w", err)
	}
	defer func() {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	}()
	return fn()
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Resolve the effective launch and recovery roots before acquiring host authority.
// Persist canonical paths so changing a symlink or a custom secret's configuration
// cannot make a successor inventory a different directory from its predecessor.
func resolveWorkerRecoveryRoots(workDir, jailerDir string) (string, string, error) {
	canonical := func(path string) (string, error) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(absolute, 0700); err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(absolute)
	}
	workDir, err := canonical(workDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve worker recovery directory: %w", err)
	}
	if strings.TrimSpace(jailerDir) == "" {
		jailerDir = filepath.Join(workDir, "vms", "jailer")
	}
	jailerDir, err = canonical(jailerDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve worker jailer directory: %w", err)
	}
	return workDir, jailerDir, nil
}
