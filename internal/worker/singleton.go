package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type ProcessIdentity struct {
	ServiceID string   `json:"service_id"`
	Roles     []string `json:"roles"`
}

type Singleton struct {
	file         *os.File
	identityPath string
}

func Acquire(workDir string, identity ProcessIdentity) (*Singleton, error) {
	if workDir == "" || identity.ServiceID == "" || len(identity.Roles) == 0 {
		return nil, errors.New("supervisor work directory, service id, and roles are required")
	}
	stateDir := filepath.Join(workDir, "supervisor")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create supervisor state directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(stateDir, "process.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open supervisor singleton lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, errors.New("another helmr worker supervisor owns this work directory")
	}
	payload, err := json.Marshal(identity)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("encode supervisor process identity: %w", err)
	}
	identityPath := filepath.Join(stateDir, "process.json")
	if err := os.WriteFile(identityPath, payload, 0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write supervisor process identity: %w", err)
	}
	return &Singleton{file: file, identityPath: identityPath}, nil
}

func ReadProcessIdentity(workDir string) (ProcessIdentity, error) {
	payload, err := os.ReadFile(filepath.Join(workDir, "supervisor", "process.json"))
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("read running supervisor identity: %w", err)
	}
	var identity ProcessIdentity
	if err := json.Unmarshal(payload, &identity); err != nil {
		return ProcessIdentity{}, fmt.Errorf("decode running supervisor identity: %w", err)
	}
	if identity.ServiceID == "" || len(identity.Roles) == 0 {
		return ProcessIdentity{}, errors.New("running supervisor identity is incomplete")
	}
	return identity, nil
}

func (s *Singleton) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	_ = os.Remove(s.identityPath)
	err := s.file.Close()
	s.file = nil
	return err
}
