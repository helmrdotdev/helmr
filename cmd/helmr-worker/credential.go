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
	"slices"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/enrollment"
	"golang.org/x/sys/unix"
)

const workerCredentialFileName = "worker-credential.json"

var buildWorkerEnrollmentRequest = enrollment.BuildAWSRequest

type workerCredentialFile struct {
	WorkerInstanceID     string    `json:"worker_instance_id"`
	WorkerInstanceSecret string    `json:"worker_instance_secret"`
	CreatedAt            time.Time `json:"created_at"`
}

func resolveWorkerInstanceCredential(ctx context.Context, cfg config.Worker, workDir string) (workerCredentialFile, error) {
	path := workerCredentialPath(workDir, cfg.WorkerInstanceCredentialPath)
	if credential, err := readWorkerInstanceCredential(path); err == nil {
		return credential, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return workerCredentialFile{}, err
	}
	var credential workerCredentialFile
	if err := withWorkerCredentialLock(path, func() error {
		if stored, err := readWorkerInstanceCredential(path); err == nil {
			credential = stored
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		controlClient, err := client.New(cfg.ControlURL)
		if err != nil {
			return fmt.Errorf("configure worker enrollment client: %w", err)
		}
		supportsRun := slices.Contains(cfg.WorkerRoles, auth.WorkerRoleRun)
		supportsBuild := slices.Contains(cfg.WorkerRoles, auth.WorkerRoleBuild)
		challenge, err := controlClient.CreateWorkerEnrollmentChallenge(ctx, cfg.WorkerGroupID)
		if err != nil {
			return fmt.Errorf("create worker enrollment challenge: %w", err)
		}
		if strings.TrimSpace(challenge.WorkerGroupID) != cfg.WorkerGroupID || strings.TrimSpace(challenge.Nonce) == "" {
			return errors.New("worker enrollment challenge is invalid")
		}
		evidence, err := buildWorkerEnrollmentRequest(ctx, cfg.WorkerGroupID, challenge.Nonce)
		if err != nil {
			return err
		}
		evidence.SupportsRun = supportsRun
		evidence.SupportsBuild = supportsBuild
		evidence.ProtocolVersion = auth.WorkerProtocolVersion
		registered, err := controlClient.EnrollWorker(ctx, evidence)
		if err != nil {
			return fmt.Errorf("enroll worker: %w", err)
		}
		registered.WorkerInstanceID = strings.TrimSpace(registered.WorkerInstanceID)
		registered.WorkerInstanceSecret = strings.TrimSpace(registered.WorkerInstanceSecret)
		if registered.WorkerInstanceID == "" {
			return errors.New("worker enrollment response worker_instance_id is empty")
		}
		if registered.WorkerInstanceSecret == "" {
			return errors.New("worker enrollment response secret is empty")
		}
		credential = workerCredentialFile{
			WorkerInstanceID:     registered.WorkerInstanceID,
			WorkerInstanceSecret: registered.WorkerInstanceSecret,
			CreatedAt:            time.Now().UTC(),
		}
		if err := writeWorkerInstanceSecret(path, credential); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return workerCredentialFile{}, err
	}
	return credential, nil
}

func resolveAuthenticatedWorkerCredential(
	ctx context.Context,
	cfg config.Worker,
	workDir string,
	authenticate func(workerCredentialFile) error,
) (workerCredentialFile, error) {
	credential, err := resolveWorkerInstanceCredential(ctx, cfg, workDir)
	if err != nil {
		return workerCredentialFile{}, err
	}
	if err := authenticate(credential); err == nil {
		return credential, nil
	} else if !client.IsStatus(err, http.StatusUnauthorized) {
		return workerCredentialFile{}, fmt.Errorf("authenticate worker credential: %w", err)
	}
	path := workerCredentialPath(workDir, cfg.WorkerInstanceCredentialPath)
	if err := removeWorkerCredentialIfMatch(path, credential); err != nil {
		return workerCredentialFile{}, err
	}
	credential, err = resolveWorkerInstanceCredential(ctx, cfg, workDir)
	if err != nil {
		return workerCredentialFile{}, fmt.Errorf("replace rejected worker credential: %w", err)
	}
	if err := authenticate(credential); err != nil {
		return workerCredentialFile{}, fmt.Errorf("authenticate replaced worker credential: %w", err)
	}
	return credential, nil
}

func removeWorkerCredentialIfMatch(path string, rejected workerCredentialFile) error {
	return withWorkerCredentialLock(path, func() error {
		current, err := readWorkerInstanceCredential(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.WorkerInstanceID != rejected.WorkerInstanceID || current.WorkerInstanceSecret != rejected.WorkerInstanceSecret {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove rejected worker instance credential: %w", err)
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return fmt.Errorf("sync rejected worker instance credential removal: %w", err)
		}
		return nil
	})
}

func resolveWorkerControlCredential(cfg config.WorkerControl, workDir string) (workerCredentialFile, error) {
	path := workerCredentialPath(workDir, cfg.WorkerInstanceCredentialPath)
	return readWorkerInstanceCredential(path)
}

func workerCredentialPath(workDir string, configured string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}
	return filepath.Join(workDir, workerCredentialFileName)
}

func readWorkerInstanceCredential(path string) (workerCredentialFile, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return workerCredentialFile{}, err
	}
	if !info.Mode().IsRegular() {
		return workerCredentialFile{}, fmt.Errorf("worker instance credential %s is not a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		return workerCredentialFile{}, fmt.Errorf("worker instance credential %s must have mode 0600", path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return workerCredentialFile{}, fmt.Errorf("open worker instance credential %s without following links: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return workerCredentialFile{}, fmt.Errorf("inspect opened worker instance credential %s: %w", path, err)
	}
	if !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 {
		return workerCredentialFile{}, fmt.Errorf("opened worker instance credential %s changed type or permissions", path)
	}
	bytes, err := io.ReadAll(file)
	if err != nil {
		return workerCredentialFile{}, fmt.Errorf("read worker instance credential %s: %w", path, err)
	}
	var credential workerCredentialFile
	if err := json.Unmarshal(bytes, &credential); err != nil {
		return workerCredentialFile{}, fmt.Errorf("read worker instance credential %s: %w", path, err)
	}
	credential.WorkerInstanceID = strings.TrimSpace(credential.WorkerInstanceID)
	credential.WorkerInstanceSecret = strings.TrimSpace(credential.WorkerInstanceSecret)
	if credential.WorkerInstanceID == "" || credential.WorkerInstanceSecret == "" {
		return workerCredentialFile{}, fmt.Errorf("worker instance credential %s is incomplete", path)
	}
	return credential, nil
}

func writeWorkerInstanceSecret(path string, credential workerCredentialFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create worker instance credential directory: %w", err)
	}
	bytes, err := json.MarshalIndent(credential, "", "  ")
	if err != nil {
		return fmt.Errorf("encode worker instance credential: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create worker instance credential temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(append(bytes, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write worker instance credential temp file: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod worker instance credential temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync worker instance credential temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close worker instance credential temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install worker instance credential file: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync worker instance credential directory: %w", err)
	}
	return nil
}

func withWorkerCredentialLock(path string, fn func() error) error {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("create worker instance credential lock directory: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open worker instance credential lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock worker instance credential: %w", err)
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
