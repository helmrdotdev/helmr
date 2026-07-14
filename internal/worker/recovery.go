package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

type RecoveryEvidence struct {
	ObservedAt       time.Time `json:"observed_at"`
	Reclaimed        []string  `json:"reclaimed,omitempty"`
	Quarantined      []string  `json:"quarantined,omitempty"`
	QuarantineErrors []string  `json:"quarantine_errors,omitempty"`
}

func (e RecoveryEvidence) HealthDetails() json.RawMessage {
	payload, err := json.Marshal(map[string]any{"startup_recovery": e})
	if err != nil {
		return json.RawMessage(`{"startup_recovery":{"error":"encode evidence"}}`)
	}
	return payload
}

type runtimeRecoveryOps struct {
	candidates     func(context.Context) ([]string, error)
	ownedProcesses func(context.Context) ([]ownedRuntimeProcess, error)
	netnsNames     func(context.Context) ([]string, error)
	matchingPIDs   func(string) ([]int, error)
	stopPID        func(context.Context, int) error
	netnsExists    func(context.Context, string) (bool, error)
	deleteNetns    func(context.Context, string) error
	removeAll      func(string) error
}

type ownedRuntimeProcess struct {
	PID     int
	ID      string
	Problem string
}

func RecoverLocalRuntimeState(ctx context.Context, workDir string, jailerDir string, ipPath string) (RecoveryEvidence, error) {
	if strings.TrimSpace(ipPath) == "" {
		ipPath = "ip"
	}
	ops := runtimeRecoveryOps{
		candidates:     func(context.Context) ([]string, error) { return runtimeCandidates(workDir, jailerDir) },
		ownedProcesses: func(context.Context) ([]ownedRuntimeProcess, error) { return ownedRuntimeProcesses(jailerDir) },
		netnsNames:     func(ctx context.Context) ([]string, error) { return runtimeNetNSNames(ctx, ipPath) },
		matchingPIDs: func(id string) ([]int, error) {
			processes, err := ownedRuntimeProcesses(jailerDir)
			if err != nil {
				return nil, err
			}
			var pids []int
			for _, process := range processes {
				if process.Problem == "" && process.ID == id {
					pids = append(pids, process.PID)
				}
			}
			return pids, nil
		},
		stopPID: stopRuntimePID,
		netnsExists: func(ctx context.Context, id string) (bool, error) {
			output, err := exec.CommandContext(ctx, ipPath, "netns", "list").Output()
			if err != nil {
				return false, err
			}
			for line := range strings.SplitSeq(string(output), "\n") {
				fields := strings.Fields(line)
				if len(fields) != 0 && fields[0] == id {
					return true, nil
				}
			}
			return false, nil
		},
		deleteNetns: func(ctx context.Context, id string) error {
			return exec.CommandContext(ctx, ipPath, "netns", "delete", id).Run()
		},
		removeAll: os.RemoveAll,
	}
	return recoverLocalRuntimeState(ctx, workDir, jailerDir, ops)
}

func recoverLocalRuntimeState(ctx context.Context, workDir string, jailerDir string, ops runtimeRecoveryOps) (RecoveryEvidence, error) {
	evidence := RecoveryEvidence{ObservedAt: time.Now().UTC()}
	liveDir := filepath.Join(workDir, "vms", "guest")
	var candidates []string
	var err error
	if ops.candidates != nil {
		candidates, err = ops.candidates(ctx)
	} else {
		entries, readErr := os.ReadDir(liveDir)
		if readErr == nil {
			for _, entry := range entries {
				candidates = append(candidates, entry.Name())
			}
		} else if !os.IsNotExist(readErr) {
			err = readErr
		}
	}
	if err != nil {
		return evidence, fmt.Errorf("inventory local runtime ownership: %w", err)
	}
	seen := make(map[string]struct{}, len(candidates))
	for _, id := range candidates {
		seen[id] = struct{}{}
	}
	if ops.ownedProcesses != nil {
		processes, processErr := ops.ownedProcesses(ctx)
		if processErr != nil {
			return evidence, fmt.Errorf("inventory owned runtime processes: %w", processErr)
		}
		for _, process := range processes {
			if process.Problem != "" || !canonicalRuntimeID(process.ID) {
				label := fmt.Sprintf("process:%d", process.PID)
				evidence.Quarantined = append(evidence.Quarantined, label)
				problem := process.Problem
				if problem == "" {
					problem = fmt.Sprintf("owned process has non-canonical runtime id %q", process.ID)
				}
				evidence.QuarantineErrors = append(evidence.QuarantineErrors, label+": "+problem)
				continue
			}
			seen[process.ID] = struct{}{}
		}
	}
	if ops.netnsNames != nil {
		if _, netnsErr := ops.netnsNames(ctx); netnsErr != nil {
			return evidence, fmt.Errorf("inventory named network namespaces: %w", netnsErr)
		}
		// A UUID-shaped namespace alone is not ownership evidence. Namespaces
		// are reconciled only after an independently owned root or process has
		// established the exact runtime ID, avoiding unrelated host netns.
	}
	candidates = candidates[:0]
	for id := range seen {
		candidates = append(candidates, id)
	}
	sort.Strings(candidates)
	for _, id := range candidates {
		if err := ctx.Err(); err != nil {
			return evidence, err
		}
		var cleanupErrs []error
		pids, err := ops.matchingPIDs(id)
		if err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("inventory process: %w", err))
		}
		for _, pid := range pids {
			if err := ops.stopPID(ctx, pid); err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("stop pid %d: %w", pid, err))
			}
		}
		exists, err := ops.netnsExists(ctx, id)
		if err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("inventory netns: %w", err))
		} else if exists {
			if err := ops.deleteNetns(ctx, id); err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("delete netns: %w", err))
			}
		}
		if len(cleanupErrs) == 0 {
			if err := ops.removeAll(filepath.Join(liveDir, id)); err != nil {
				cleanupErrs = append(cleanupErrs, err)
			}
			if jailerDir != "" {
				if err := ops.removeAll(filepath.Join(jailerDir, "firecracker", id)); err != nil {
					cleanupErrs = append(cleanupErrs, err)
				}
			}
		}
		if len(cleanupErrs) == 0 {
			evidence.Reclaimed = append(evidence.Reclaimed, id)
			continue
		}
		evidence.Quarantined = append(evidence.Quarantined, id)
		evidence.QuarantineErrors = append(evidence.QuarantineErrors, id+": "+errors.Join(cleanupErrs...).Error())
	}
	return evidence, nil
}

func runtimeNetNSNames(ctx context.Context, ipPath string) ([]string, error) {
	output, err := exec.CommandContext(ctx, ipPath, "netns", "list").Output()
	if err != nil {
		return nil, err
	}
	var names []string
	for line := range strings.SplitSeq(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 0 {
			names = append(names, fields[0])
		}
	}
	return names, nil
}

func ownedRuntimeProcesses(jailerDir string) ([]ownedRuntimeProcess, error) {
	entries, err := os.ReadDir("/proc")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var processes []ownedRuntimeProcess
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil {
			continue
		}
		cmdline, readErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if readErr != nil {
			continue
		}
		root, _ := os.Readlink(filepath.Join("/proc", entry.Name(), "root"))
		id, owned, problem := helmrOwnedRuntimeProcess(cmdline, root, jailerDir)
		if owned {
			processes = append(processes, ownedRuntimeProcess{PID: pid, ID: id, Problem: problem})
		}
	}
	return processes, nil
}

func helmrOwnedRuntimeProcess(cmdline []byte, processRoot string, jailerDir string) (string, bool, string) {
	args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	if len(args) == 0 || strings.TrimSpace(jailerDir) == "" {
		return "", false, ""
	}
	jailerDir = filepath.Clean(jailerDir)
	switch filepath.Base(args[0]) {
	case "jailer":
		if filepath.Clean(commandFlag(args[1:], "--chroot-base-dir")) != jailerDir {
			return "", false, ""
		}
		id := commandFlag(args[1:], "--id")
		if !canonicalRuntimeID(id) {
			return id, true, fmt.Sprintf("owned jailer process has non-canonical --id %q", id)
		}
		return id, true, ""
	case "firecracker":
		prefix := filepath.Join(jailerDir, "firecracker") + string(os.PathSeparator)
		cleanRoot := filepath.Clean(strings.TrimSuffix(processRoot, " (deleted)"))
		if !strings.HasPrefix(cleanRoot, prefix) {
			return "", false, ""
		}
		rel, err := filepath.Rel(filepath.Join(jailerDir, "firecracker"), cleanRoot)
		if err != nil {
			return "", true, "cannot correlate owned firecracker root"
		}
		parts := strings.Split(rel, string(os.PathSeparator))
		id := parts[0]
		if len(parts) < 2 || parts[1] != "root" || !canonicalRuntimeID(id) {
			return id, true, fmt.Sprintf("owned firecracker root has non-canonical runtime identity %q", rel)
		}
		return id, true, ""
	default:
		return "", false, ""
	}
}

func commandFlag(args []string, name string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
		if value, ok := strings.CutPrefix(arg, name+"="); ok {
			return value
		}
	}
	return ""
}

var runtimeIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func canonicalRuntimeID(name string) bool {
	if !runtimeIDPattern.MatchString(name) {
		return false
	}
	id, err := uuid.Parse(name)
	return err == nil && id.String() == name
}

func runtimeCandidates(workDir string, jailerDir string) ([]string, error) {
	seen := map[string]struct{}{}
	addRoot := func(path string) error {
		entries, err := os.ReadDir(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if !canonicalRuntimeID(name) {
				return fmt.Errorf("runtime ownership root %s contains non-canonical runtime id %q", path, name)
			}
			seen[name] = struct{}{}
		}
		return nil
	}
	if err := addRoot(filepath.Join(workDir, "vms", "guest")); err != nil {
		return nil, err
	}
	if jailerDir != "" {
		if err := addRoot(filepath.Join(jailerDir, "firecracker")); err != nil {
			return nil, err
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func stopRuntimePID(ctx context.Context, pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		if err := process.Signal(syscall.Signal(0)); errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return process.Kill()
		case <-ticker.C:
		}
	}
}
