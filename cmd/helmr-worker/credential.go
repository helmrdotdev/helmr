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
	registrationToken, cleanupBootstrapToken, err := workerBootstrapToken(cfg)
	if err != nil {
		return workerCredentialFile{}, err
	}
	if registrationToken == "" {
		return workerCredentialFile{}, fmt.Errorf("worker instance credential not found at %s and neither HELMR_WORKER_BOOTSTRAP_TOKEN nor HELMR_WORKER_BOOTSTRAP_TOKEN_PATH is set", path)
	}
	controlClient, err := client.New(cfg.ControlURL)
	if err != nil {
		return workerCredentialFile{}, fmt.Errorf("configure registration client: %w", err)
	}
	registered, err := controlClient.RegisterWorker(ctx, registrationToken, workerResourceID(cfg))
	if err != nil {
		return workerCredentialFile{}, fmt.Errorf("register worker: %w", err)
	}
	registered.WorkerInstanceID = strings.TrimSpace(registered.WorkerInstanceID)
	registered.WorkerInstanceSecret = strings.TrimSpace(registered.WorkerInstanceSecret)
	if registered.WorkerInstanceID == "" {
		return workerCredentialFile{}, errors.New("worker bootstrap response worker_instance_id is empty")
	}
	if registered.WorkerInstanceSecret == "" {
		return workerCredentialFile{}, errors.New("worker bootstrap response secret is empty")
	}
	credential := workerCredentialFile{
		WorkerInstanceID:     registered.WorkerInstanceID,
		WorkerInstanceSecret: registered.WorkerInstanceSecret,
		CreatedAt:            time.Now().UTC(),
	}
	if err := writeWorkerInstanceSecret(path, credential); err != nil {
		return workerCredentialFile{}, err
	}
	cleanupBootstrapToken()
	return credential, nil
}

func workerBootstrapToken(cfg config.Worker) (string, func(), error) {
	if token := strings.TrimSpace(cfg.WorkerBootstrapToken); token != "" {
		return token, func() {}, nil
	}
	path := strings.TrimSpace(cfg.WorkerBootstrapTokenPath)
	if path == "" {
		return "", func() {}, nil
	}
	bytes, err := os.ReadFile(path)
	if err != nil {
		return "", func() {}, fmt.Errorf("read worker bootstrap token: %w", err)
	}
	return strings.TrimSpace(string(bytes)), func() {
		_ = os.Remove(path)
	}, nil
}

func workerResourceID(cfg config.Worker) string {
	return strings.TrimSpace(cfg.WorkerResourceID)
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
	bytes, err := os.ReadFile(path)
	if err != nil {
		return workerCredentialFile{}, err
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
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close worker instance credential temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install worker instance credential file: %w", err)
	}
	return nil
}
