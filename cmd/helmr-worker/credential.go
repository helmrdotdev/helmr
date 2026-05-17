package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/config"
)

const workerCredentialFileName = "worker-credential.json"

type workerCredentialFile struct {
	WorkerID     string    `json:"worker_id"`
	WorkerSecret string    `json:"worker_secret"`
	CreatedAt    time.Time `json:"created_at"`
}

func resolveWorkerCredential(ctx context.Context, cfg config.Worker, workDir string) (workerCredentialFile, error) {
	if secret := strings.TrimSpace(cfg.WorkerSecret); secret != "" {
		return workerCredentialFile{
			WorkerID:     strings.TrimSpace(cfg.WorkerID),
			WorkerSecret: secret,
		}, nil
	}
	path := workerCredentialPath(workDir, cfg.WorkerCredentialPath)
	if credential, err := readWorkerCredential(path); err == nil {
		return credential, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return workerCredentialFile{}, err
	}
	registrationToken, cleanupRegistrationToken, err := workerPoolRegistrationToken(cfg)
	if err != nil {
		return workerCredentialFile{}, err
	}
	if registrationToken == "" {
		return workerCredentialFile{}, fmt.Errorf("worker credential not found at %s and neither HELMR_WORKER_POOL_REGISTRATION_TOKEN nor HELMR_WORKER_POOL_REGISTRATION_TOKEN_PATH is set", path)
	}
	controlClient, err := client.New(cfg.ControlURL)
	if err != nil {
		return workerCredentialFile{}, fmt.Errorf("configure registration client: %w", err)
	}
	registered, err := controlClient.RegisterWorker(ctx, registrationToken, workerResourceName(cfg))
	if err != nil {
		return workerCredentialFile{}, fmt.Errorf("register worker: %w", err)
	}
	registered.WorkerID = strings.TrimSpace(registered.WorkerID)
	registered.WorkerSecret = strings.TrimSpace(registered.WorkerSecret)
	if registered.WorkerID == "" {
		return workerCredentialFile{}, errors.New("worker registration response worker_id is empty")
	}
	if registered.WorkerSecret == "" {
		return workerCredentialFile{}, errors.New("worker registration response secret is empty")
	}
	credential := workerCredentialFile{
		WorkerID:     registered.WorkerID,
		WorkerSecret: registered.WorkerSecret,
		CreatedAt:    time.Now().UTC(),
	}
	if err := writeWorkerSecret(path, credential); err != nil {
		return workerCredentialFile{}, err
	}
	cleanupRegistrationToken()
	return credential, nil
}

func workerPoolRegistrationToken(cfg config.Worker) (string, func(), error) {
	if token := strings.TrimSpace(cfg.WorkerPoolRegistrationToken); token != "" {
		return token, func() {}, nil
	}
	path := strings.TrimSpace(cfg.WorkerPoolRegistrationTokenPath)
	if path == "" {
		return "", func() {}, nil
	}
	bytes, err := os.ReadFile(path)
	if err != nil {
		return "", func() {}, fmt.Errorf("read worker pool registration token: %w", err)
	}
	return strings.TrimSpace(string(bytes)), func() {
		_ = os.Remove(path)
	}, nil
}

func workerResourceName(cfg config.Worker) string {
	return strings.TrimSpace(cfg.WorkerID)
}

func resolveWorkerControlCredential(cfg config.WorkerControl, workDir string) (workerCredentialFile, error) {
	if secret := strings.TrimSpace(cfg.WorkerSecret); secret != "" {
		return workerCredentialFile{
			WorkerID:     strings.TrimSpace(cfg.WorkerID),
			WorkerSecret: secret,
		}, nil
	}
	path := workerCredentialPath(workDir, cfg.WorkerCredentialPath)
	return readWorkerCredential(path)
}

func workerCredentialPath(workDir string, configured string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}
	return filepath.Join(workDir, workerCredentialFileName)
}

func readWorkerCredential(path string) (workerCredentialFile, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return workerCredentialFile{}, err
	}
	var credential workerCredentialFile
	if err := json.Unmarshal(bytes, &credential); err != nil {
		return workerCredentialFile{}, fmt.Errorf("read worker credential %s: %w", path, err)
	}
	credential.WorkerID = strings.TrimSpace(credential.WorkerID)
	credential.WorkerSecret = strings.TrimSpace(credential.WorkerSecret)
	if credential.WorkerID == "" || credential.WorkerSecret == "" {
		return workerCredentialFile{}, fmt.Errorf("worker credential %s is incomplete", path)
	}
	return credential, nil
}

func writeWorkerSecret(path string, credential workerCredentialFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create worker credential directory: %w", err)
	}
	bytes, err := json.MarshalIndent(credential, "", "  ")
	if err != nil {
		return fmt.Errorf("encode worker credential: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create worker credential temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(append(bytes, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write worker credential temp file: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod worker credential temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close worker credential temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install worker credential file: %w", err)
	}
	return nil
}
