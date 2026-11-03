package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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

const workerIdentityFileName = "worker-identity.json"

// workerIdentityFile is the machine's durable identity: the host it enrolled
// as and the Ed25519 machine key it proves itself with. The private key never
// leaves this root-only file.
type workerIdentityFile struct {
	WorkerHostID string    `json:"worker_host_id"`
	MachineKey   []byte    `json:"machine_key"`
	CreatedAt    time.Time `json:"created_at"`
	WorkDir      string    `json:"work_dir"`
	JailerDir    string    `json:"jailer_dir"`
}

func (identity workerIdentityFile) key() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(identity.MachineKey)
}

func resolveWorkerIdentity(ctx context.Context, cfg config.Worker, workDir string) (workerIdentityFile, error) {
	workDir, jailerDir, err := resolveWorkerRecoveryRoots(workDir, cfg.JailerChrootDir)
	if err != nil {
		return workerIdentityFile{}, err
	}
	validate := func(identity workerIdentityFile) error {
		if identity.WorkDir != workDir || identity.JailerDir != jailerDir {
			return errors.New("worker identity recovery roots are missing or do not match configured roots")
		}
		return nil
	}
	path := workerIdentityPath(workDir, cfg.WorkerIdentityPath)
	if identity, err := readWorkerIdentity(path); err == nil {
		if _, err := os.Lstat(pendingMachineKeyPath(path)); err == nil {
			// A pending key left by a crash after the identity was stored
			// belongs to that identity and must not outlive it.
			if err := withWorkerIdentityLock(path, func() error { return removePendingMachineKey(path) }); err != nil {
				return workerIdentityFile{}, err
			}
		}
		return identity, validate(identity)
	} else if !errors.Is(err, os.ErrNotExist) {
		return workerIdentityFile{}, err
	}
	var identity workerIdentityFile
	if err := withWorkerIdentityLock(path, func() error {
		if stored, err := readWorkerIdentity(path); err == nil {
			// A pending key left by a crash after the identity was stored
			// belongs to that identity and must not outlive it.
			if err := removePendingMachineKey(path); err != nil {
				return err
			}
			identity = stored
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
		// Each enrollment gets a new machine key: a key registered once is
		// never accepted again. The key is stored before enrolling, so a
		// lost response or a crash repeats the same enrollment, which the
		// Control Plane answers with the same host.
		private, err := pendingMachineKey(path)
		if err != nil {
			return err
		}
		enroll := func(key ed25519.PrivateKey) (workerapi.EnrollmentResponse, error) {
			return controlPlaneClient.EnrollWorker(ctx, enrollmentToken, workerapi.EnrollmentRequest{
				ResourceID: cfg.WorkerResourceID, PoolName: cfg.WorkerPoolName, PublicKey: key.Public().(ed25519.PublicKey),
			})
		}
		registered, err := enroll(private)
		var refused *httpclient.Error
		if errors.As(err, &refused) && refused.Code == "worker_key_registered" {
			// The pending key was registered by an enrollment whose host has
			// since been fenced or revoked; a key is never accepted twice.
			if err := removePendingMachineKey(path); err != nil {
				return err
			}
			if private, err = pendingMachineKey(path); err != nil {
				return err
			}
			registered, err = enroll(private)
		}
		if err != nil {
			return fmt.Errorf("enroll worker: %w", err)
		}
		registered.WorkerHostID = strings.TrimSpace(registered.WorkerHostID)
		registered.WorkerPoolID = strings.TrimSpace(registered.WorkerPoolID)
		if registered.WorkerHostID == "" {
			return errors.New("worker enrollment response worker_host_id is empty")
		}
		if _, err := ids.Parse(registered.WorkerPoolID); err != nil {
			return errors.New("worker enrollment response worker_pool_id is not a canonical UUIDv7")
		}
		identity = workerIdentityFile{
			WorkerHostID: registered.WorkerHostID,
			MachineKey:   private.Seed(),
			CreatedAt:    time.Now().UTC(),
			WorkDir:      workDir,
			JailerDir:    jailerDir,
		}
		if err := writeWorkerIdentity(path, identity); err != nil {
			return err
		}
		return removePendingMachineKey(path)
	}); err != nil {
		return workerIdentityFile{}, err
	}
	return identity, nil
}

// errWorkerKeyRejected means the Control Plane no longer accepts this machine
// key: it was revoked, or its host was declared permanently lost. The worker
// stops rather than enrolling itself again; coming back takes a new
// enrollment with a valid token.
var errWorkerKeyRejected = errors.New("the Control Plane rejected this machine's key; it was revoked or its host was removed. Remove the worker identity and enroll this machine again with a valid enrollment token")

func resolveAuthenticatedWorkerIdentity(
	ctx context.Context,
	cfg config.Worker,
	workDir string,
	authenticate func(workerIdentityFile) error,
) (workerIdentityFile, error) {
	identity, err := resolveWorkerIdentity(ctx, cfg, workDir)
	if err != nil {
		return workerIdentityFile{}, err
	}
	if err := authenticate(identity); err != nil {
		if httpclient.IsStatus(err, http.StatusUnauthorized) {
			return workerIdentityFile{}, errWorkerKeyRejected
		}
		return workerIdentityFile{}, fmt.Errorf("authenticate worker identity: %w", err)
	}
	return identity, nil
}

func resolveWorkerControlPlaneIdentity(cfg config.WorkerControlPlane, workDir string) (workerIdentityFile, error) {
	path := workerIdentityPath(workDir, cfg.WorkerIdentityPath)
	return readWorkerIdentity(path)
}

func workerIdentityPath(workDir string, configured string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}
	return filepath.Join(workDir, workerIdentityFileName)
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

func pendingMachineKeyPath(identityPath string) string {
	return identityPath + ".pending"
}

func removePendingMachineKey(identityPath string) error {
	if err := os.Remove(pendingMachineKeyPath(identityPath)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove pending worker machine key: %w", err)
	}
	return syncDirectory(filepath.Dir(identityPath))
}

// pendingMachineKey returns the machine key an unfinished enrollment used, or
// generates and stores a new one. The caller holds the identity lock.
func pendingMachineKey(identityPath string) (ed25519.PrivateKey, error) {
	path := pendingMachineKeyPath(identityPath)
	pending, err := readWorkerIdentityFile(path)
	if err == nil && len(pending.MachineKey) == ed25519.SeedSize {
		return pending.key(), nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate worker machine key: %w", err)
	}
	if err := writeWorkerIdentity(path, workerIdentityFile{MachineKey: private.Seed(), CreatedAt: time.Now().UTC()}); err != nil {
		return nil, err
	}
	return private, nil
}

func readWorkerIdentity(path string) (workerIdentityFile, error) {
	identity, err := readWorkerIdentityFile(path)
	if err != nil {
		return workerIdentityFile{}, err
	}
	identity.WorkerHostID = strings.TrimSpace(identity.WorkerHostID)
	if identity.WorkerHostID == "" || len(identity.MachineKey) != ed25519.SeedSize {
		return workerIdentityFile{}, fmt.Errorf("worker identity %s is incomplete", path)
	}
	return identity, nil
}

func readWorkerIdentityFile(path string) (workerIdentityFile, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return workerIdentityFile{}, err
	}
	if !info.Mode().IsRegular() {
		return workerIdentityFile{}, fmt.Errorf("worker identity %s is not a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		return workerIdentityFile{}, fmt.Errorf("worker identity %s must have mode 0600", path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return workerIdentityFile{}, fmt.Errorf("open worker identity %s without following links: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return workerIdentityFile{}, fmt.Errorf("inspect opened worker identity %s: %w", path, err)
	}
	if !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 {
		return workerIdentityFile{}, fmt.Errorf("opened worker identity %s changed type or permissions", path)
	}
	bytes, err := io.ReadAll(file)
	if err != nil {
		return workerIdentityFile{}, fmt.Errorf("read worker identity %s: %w", path, err)
	}
	var identity workerIdentityFile
	if err := json.Unmarshal(bytes, &identity); err != nil {
		return workerIdentityFile{}, fmt.Errorf("read worker identity %s: %w", path, err)
	}
	return identity, nil
}

func writeWorkerIdentity(path string, identity workerIdentityFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create worker identity directory: %w", err)
	}
	bytes, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return fmt.Errorf("encode worker identity: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create worker identity temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(append(bytes, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write worker identity temp file: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod worker identity temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync worker identity temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close worker identity temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install worker identity file: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync worker identity directory: %w", err)
	}
	return nil
}

func withWorkerIdentityLock(path string, fn func() error) error {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("create worker identity lock directory: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open worker identity lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock worker identity: %w", err)
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
// Persist canonical paths so changing a symlink or a custom identity path
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
