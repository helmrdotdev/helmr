package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type RecoveryEvidence struct {
	ObservedAt        time.Time  `json:"observed_at"`
	Reclaimed         []string   `json:"reclaimed,omitempty"`
	ReclaimedOwners   []vm.Owner `json:"reclaimed_owners,omitempty"`
	Quarantined       []string   `json:"quarantined,omitempty"`
	QuarantinedOwners []vm.Owner `json:"quarantined_owners,omitempty"`
	QuarantineErrors  []string   `json:"quarantine_errors,omitempty"`
}

func (e RecoveryEvidence) HealthDetails() json.RawMessage {
	payload, err := json.Marshal(map[string]any{"startup_recovery": e})
	if err != nil {
		return json.RawMessage(`{"startup_recovery":{"error":"encode evidence"}}`)
	}
	return payload
}

type vmRecoveryOps struct {
	ownerCandidates func(context.Context) ([]ownerCandidate, error)
	ownedProcesses  func(context.Context) ([]ownedVMProcess, error)
	netnsNames      func(context.Context) ([]string, error)
	matchingPIDs    func(string) ([]int, error)
	stopPID         func(context.Context, int) error
	netnsExists     func(context.Context, string) (bool, error)
	deleteNetns     func(context.Context, string) error
	removeAll       func(string) error
	removeState     func(string, vm.Owner) error
}

type ownerCandidate struct {
	Owner   vm.Owner
	Label   string
	Problem string
}

type ownedVMProcess struct {
	PID     int
	ID      string
	Problem string
}

func RecoverLocalVMState(ctx context.Context, workDir string, jailerDir string, ipPath string) (RecoveryEvidence, error) {
	if strings.TrimSpace(ipPath) == "" {
		ipPath = "ip"
	}
	ops := vmRecoveryOps{
		ownerCandidates: func(context.Context) ([]ownerCandidate, error) { return ownedVMCandidates(workDir, jailerDir) },
		ownedProcesses:  func(context.Context) ([]ownedVMProcess, error) { return ownedVMProcesses(jailerDir) },
		netnsNames:      func(ctx context.Context) ([]string, error) { return vmNetNSNames(ctx, ipPath) },
		matchingPIDs: func(id string) ([]int, error) {
			processes, err := ownedVMProcesses(jailerDir)
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
		stopPID: stopVMPID,
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
		removeAll:   os.RemoveAll,
		removeState: removeOwnedRecoveryState,
	}
	return recoverLocalVMState(ctx, workDir, jailerDir, ops)
}

func recoverLocalVMState(ctx context.Context, workDir string, jailerDir string, ops vmRecoveryOps) (RecoveryEvidence, error) {
	evidence := RecoveryEvidence{ObservedAt: time.Now().UTC()}
	liveDir := filepath.Join(workDir, "vms", "guest")
	var candidates []string
	owners := make(map[string]vm.Owner)
	var err error
	if ops.ownerCandidates != nil {
		owned, ownerErr := ops.ownerCandidates(ctx)
		if ownerErr != nil {
			err = ownerErr
		} else {
			rejected := make(map[string]struct{})
			for _, candidate := range owned {
				label := candidate.Label
				if label == "" {
					label = candidate.Owner.String()
				}
				if candidate.Problem != "" {
					if candidate.Owner.ID != "" {
						rejected[candidate.Owner.ID] = struct{}{}
						delete(owners, candidate.Owner.ID)
					}
					evidence.Quarantined = append(evidence.Quarantined, label)
					evidence.QuarantineErrors = append(evidence.QuarantineErrors, label+": "+candidate.Problem)
					continue
				}
				if previous, ok := owners[candidate.Owner.ID]; ok && previous != candidate.Owner {
					rejected[candidate.Owner.ID] = struct{}{}
					delete(owners, candidate.Owner.ID)
					evidence.Quarantined = append(evidence.Quarantined, label)
					evidence.QuarantineErrors = append(evidence.QuarantineErrors, label+": conflicting ownership evidence")
					continue
				}
				if _, ok := rejected[candidate.Owner.ID]; ok {
					continue
				}
				owners[candidate.Owner.ID] = candidate.Owner
			}
			for id := range owners {
				candidates = append(candidates, id)
			}
		}
	} else {
		return evidence, errors.New("VM owner inventory is required")
	}
	if err != nil {
		return evidence, fmt.Errorf("inventory local VM ownership: %w", err)
	}
	seen := make(map[string]struct{}, len(candidates))
	for _, id := range candidates {
		seen[id] = struct{}{}
	}
	if ops.ownedProcesses != nil {
		processes, processErr := ops.ownedProcesses(ctx)
		if processErr != nil {
			return evidence, fmt.Errorf("inventory owned VM processes: %w", processErr)
		}
		for _, process := range processes {
			if process.Problem != "" || !canonicalVMID(process.ID) {
				label := fmt.Sprintf("process:%d", process.PID)
				evidence.Quarantined = append(evidence.Quarantined, label)
				problem := process.Problem
				if problem == "" {
					problem = fmt.Sprintf("owned process has non-canonical VM id %q", process.ID)
				}
				evidence.QuarantineErrors = append(evidence.QuarantineErrors, label+": "+problem)
				continue
			}
			if ops.ownerCandidates != nil {
				if _, ok := owners[process.ID]; !ok {
					label := fmt.Sprintf("process:%d", process.PID)
					evidence.Quarantined = append(evidence.Quarantined, label)
					evidence.QuarantineErrors = append(evidence.QuarantineErrors, label+": exact VM owner marker is missing")
					continue
				}
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
		owner, hasOwner := owners[id]
		if hasOwner && len(cleanupErrs) == 0 {
			remaining, verifyErr := ops.matchingPIDs(id)
			if verifyErr != nil || len(remaining) != 0 {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("verify process absence: pids=%v: %v", remaining, verifyErr))
			}
			exists, verifyErr = ops.netnsExists(ctx, id)
			if verifyErr != nil || exists {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("verify netns absence: exists=%t: %v", exists, verifyErr))
			}
		}
		if len(cleanupErrs) == 0 {
			if jailerDir != "" {
				if err := ops.removeAll(filepath.Join(jailerDir, "firecracker", id)); err != nil {
					cleanupErrs = append(cleanupErrs, err)
				}
				if _, err := os.Lstat(filepath.Join(jailerDir, "firecracker", id)); !os.IsNotExist(err) {
					cleanupErrs = append(cleanupErrs, fmt.Errorf("verify jailer state absence: %v", err))
				}
			}
		}
		if len(cleanupErrs) == 0 {
			statePath := filepath.Join(liveDir, id)
			if hasOwner && ops.removeState != nil {
				if err := ops.removeState(statePath, owner); err != nil {
					cleanupErrs = append(cleanupErrs, err)
				}
			} else if err := ops.removeAll(statePath); err != nil {
				cleanupErrs = append(cleanupErrs, err)
			}
		}
		if len(cleanupErrs) == 0 {
			label := id
			if hasOwner {
				label = owner.String()
				evidence.ReclaimedOwners = append(evidence.ReclaimedOwners, owner)
			}
			evidence.Reclaimed = append(evidence.Reclaimed, label)
			continue
		}
		label := id
		if hasOwner {
			label = owner.String()
			evidence.QuarantinedOwners = append(evidence.QuarantinedOwners, owner)
		}
		evidence.Quarantined = append(evidence.Quarantined, label)
		evidence.QuarantineErrors = append(evidence.QuarantineErrors, label+": "+errors.Join(cleanupErrs...).Error())
	}
	return evidence, nil
}

func vmNetNSNames(ctx context.Context, ipPath string) ([]string, error) {
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

func ownedVMProcesses(jailerDir string) ([]ownedVMProcess, error) {
	entries, err := os.ReadDir("/proc")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var processes []ownedVMProcess
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
		id, owned, problem := helmrOwnedVMProcess(cmdline, root, jailerDir)
		if owned {
			processes = append(processes, ownedVMProcess{PID: pid, ID: id, Problem: problem})
		}
	}
	return processes, nil
}

func helmrOwnedVMProcess(cmdline []byte, processRoot string, jailerDir string) (string, bool, string) {
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
		if !canonicalVMID(id) {
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
		if len(parts) < 2 || parts[1] != "root" || !canonicalVMID(id) {
			return id, true, fmt.Sprintf("owned firecracker root has non-canonical VM identity %q", rel)
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

func canonicalVMID(name string) bool {
	return ids.Validate(name) == nil
}

func ownedVMCandidates(workDir string, jailerDir string) ([]ownerCandidate, error) {
	liveDir := filepath.Join(workDir, "vms", "guest")
	entries, err := os.ReadDir(liveDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var candidates []ownerCandidate
	owned := make(map[string]vm.Owner)
	for _, entry := range entries {
		id := entry.Name()
		label := "state:" + id
		if !canonicalVMID(id) {
			candidates = append(candidates, ownerCandidate{Label: label, Problem: fmt.Sprintf("state root has non-canonical VM id %q", id)})
			continue
		}
		marker, readErr := os.ReadFile(filepath.Join(liveDir, id, "owner"))
		if readErr != nil {
			candidates = append(candidates, ownerCandidate{Owner: vm.Owner{ID: id}, Label: label, Problem: "read exact VM owner marker: " + readErr.Error()})
			continue
		}
		owner, parseErr := parseOwnerMarker(marker)
		if parseErr != nil {
			candidates = append(candidates, ownerCandidate{Owner: vm.Owner{ID: id}, Label: label, Problem: parseErr.Error()})
			continue
		}
		if owner.ID != id {
			candidates = append(candidates, ownerCandidate{Owner: owner, Label: label, Problem: fmt.Sprintf("owner marker id %q conflicts with state root id %q", owner.ID, id)})
			continue
		}
		owned[id] = owner
		candidates = append(candidates, ownerCandidate{Owner: owner})
	}
	if strings.TrimSpace(jailerDir) == "" {
		return candidates, nil
	}
	jailerRoot := filepath.Join(jailerDir, "firecracker")
	jailerEntries, err := os.ReadDir(jailerRoot)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range jailerEntries {
		id := entry.Name()
		if !canonicalVMID(id) {
			candidates = append(candidates, ownerCandidate{Label: "jailer:" + id, Problem: fmt.Sprintf("jailer root has non-canonical VM id %q", id)})
			continue
		}
		if _, ok := owned[id]; !ok {
			candidates = append(candidates, ownerCandidate{Owner: vm.Owner{ID: id}, Label: "jailer:" + id, Problem: "exact VM owner marker is missing"})
		}
	}
	return candidates, nil
}

func parseOwnerMarker(marker []byte) (vm.Owner, error) {
	lines := strings.Split(string(marker), "\n")
	if len(lines) != 3 || lines[2] != "" {
		return vm.Owner{}, errors.New("VM owner marker has invalid format")
	}
	owner := vm.Owner{Kind: vm.OwnerKind(lines[0]), ID: lines[1]}
	if err := owner.Validate(); err != nil {
		return vm.Owner{}, err
	}
	return owner, nil
}

func removeOwnedRecoveryState(statePath string, owner vm.Owner) error {
	marker, err := os.ReadFile(filepath.Join(statePath, "owner"))
	if err != nil {
		return fmt.Errorf("read VM owner marker before cleanup: %w", err)
	}
	recorded, err := parseOwnerMarker(marker)
	if err != nil {
		return err
	}
	if recorded != owner {
		return fmt.Errorf("VM owner marker changed from %s to %s", owner, recorded)
	}
	entries, err := os.ReadDir(statePath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "owner" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(statePath, entry.Name())); err != nil {
			return err
		}
	}
	markerPath := filepath.Join(statePath, "owner")
	if err := os.Remove(markerPath); err != nil {
		return err
	}
	if err := os.Remove(statePath); err != nil {
		restoreErr := os.WriteFile(markerPath, marker, 0o600)
		return errors.Join(err, restoreErr)
	}
	return nil
}

func stopVMPID(ctx context.Context, pid int) error {
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
