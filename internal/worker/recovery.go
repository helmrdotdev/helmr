package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/firecracker/custody"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type RecoveryEvidence struct {
	ObservedAt        time.Time  `json:"observed_at"`
	Reclaimed         []string   `json:"reclaimed,omitempty"`
	Quarantined       []string   `json:"quarantined,omitempty"`
	QuarantinedOwners []vm.Owner `json:"quarantined_owners,omitempty"`
	QuarantineErrors  []string   `json:"quarantine_errors,omitempty"`
}

type vmRecoveryOps struct {
	ownerCandidates func(context.Context) ([]ownerCandidate, error)
	ownedProcesses  func(context.Context) ([]custody.Process, error)
	netnsNames      func(context.Context) ([]string, error)
	matchingPIDs    func(string) ([]int, error)
	stopPID         func(context.Context, vm.Owner, int) error
	netnsExists     func(context.Context, string) (bool, error)
	reclaimNetwork  func(context.Context, vm.Owner) error
	reclaimCgroup   func(vm.Owner) error
	removeAll       func(string) error
	removeState     func(string, vm.Owner) error
}

type ownerCandidate struct {
	Owner   vm.Owner
	Label   string
	Problem string
}

func RecoverLocalVMState(ctx context.Context, workDir string, jailerDir string, ipPath string, reclaimNetwork func(context.Context, vm.Owner) error, reclaimCgroup func(vm.Owner) error) (RecoveryEvidence, error) {
	if strings.TrimSpace(ipPath) == "" {
		ipPath = "ip"
	}
	if reclaimNetwork == nil {
		return RecoveryEvidence{}, errors.New("exact network reclaimer is required")
	}
	if reclaimCgroup == nil {
		return RecoveryEvidence{}, errors.New("exact cgroup reclaimer is required")
	}
	state := custody.StateRoot{Base: workDir, Components: []string{"vms", "guest"}}
	ops := vmRecoveryOps{
		ownerCandidates: func(context.Context) ([]ownerCandidate, error) { return ownedVMCandidates(workDir, jailerDir) },
		ownedProcesses:  func(context.Context) ([]custody.Process, error) { return custody.Processes(state, jailerDir) },
		netnsNames:      func(ctx context.Context) ([]string, error) { return vmNetNSNames(ctx, ipPath) },
		matchingPIDs: func(id string) ([]int, error) {
			return custody.MatchingPIDs(state, jailerDir, vm.Owner{Kind: vm.OwnerInstance, ID: id})
		},
		stopPID: func(ctx context.Context, owner vm.Owner, pid int) error {
			return custody.Stop(ctx, state, jailerDir, owner, pid)
		},
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
		reclaimNetwork: reclaimNetwork,
		reclaimCgroup:  reclaimCgroup,
		removeAll:      os.RemoveAll,
		removeState:    removeOwnedRecoveryState,
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
		rejectedProcesses := make(map[string]struct{})
		for _, process := range processes {
			if process.Problem != "" || !canonicalVMID(process.ID) {
				problem := process.Problem
				if problem == "" {
					problem = fmt.Sprintf("owned process has non-canonical VM id %q", process.ID)
				}
				label := fmt.Sprintf("process:%d", process.PID)
				if owner, known := owners[process.ID]; known {
					rejectedProcesses[process.ID] = struct{}{}
					label = fmt.Sprintf("%s: process %d", owner, process.PID)
				} else {
					evidence.Quarantined = append(evidence.Quarantined, label)
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
		for id := range rejectedProcesses {
			delete(seen, id)
			evidence.Quarantined = append(evidence.Quarantined, id)
			evidence.QuarantinedOwners = append(evidence.QuarantinedOwners, owners[id])
		}
	}
	if ops.netnsNames != nil {
		if _, netnsErr := ops.netnsNames(ctx); netnsErr != nil {
			return evidence, fmt.Errorf("inventory named network namespaces: %w", netnsErr)
		}
		// A UUID-shaped namespace alone is not ownership evidence. Namespaces
		// are reconciled only after an independently owned root or process has
		// established the exact instance ID, avoiding unrelated host netns.
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
		owner, hasOwner := owners[id]
		if !hasOwner {
			evidence.Quarantined = append(evidence.Quarantined, id)
			evidence.QuarantineErrors = append(evidence.QuarantineErrors, id+": exact VM owner is unavailable")
			continue
		}
		var cleanupErrs []error
		pids, err := ops.matchingPIDs(id)
		if err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("inventory process: %w", err))
		}
		for _, pid := range pids {
			if err := ops.stopPID(ctx, owner, pid); err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("stop pid %d: %w", pid, err))
			}
		}
		if ops.reclaimNetwork == nil {
			cleanupErrs = append(cleanupErrs, errors.New("exact network reclaimer is unavailable"))
		} else if err := ops.reclaimNetwork(ctx, owner); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("reclaim exact network attachment: %w", err))
		}
		if len(cleanupErrs) == 0 {
			remaining, verifyErr := ops.matchingPIDs(id)
			if verifyErr != nil || len(remaining) != 0 {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("verify process absence: pids=%v: %v", remaining, verifyErr))
			}
			exists, verifyErr := ops.netnsExists(ctx, id)
			if verifyErr != nil || exists {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("verify netns absence: exists=%t: %v", exists, verifyErr))
			}
		}
		if len(cleanupErrs) == 0 {
			if ops.reclaimCgroup == nil {
				cleanupErrs = append(cleanupErrs, errors.New("exact cgroup reclaimer is unavailable"))
			} else if err := ops.reclaimCgroup(owner); err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("prove exact cgroup absence: %w", err))
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
			if ops.removeState != nil {
				if err := ops.removeState(workDir, owner); err != nil {
					cleanupErrs = append(cleanupErrs, err)
				}
			} else if err := ops.removeAll(statePath); err != nil {
				cleanupErrs = append(cleanupErrs, err)
			}
		}
		if len(cleanupErrs) == 0 {
			evidence.Reclaimed = append(evidence.Reclaimed, id)
			continue
		}
		label := owner.String()
		evidence.QuarantinedOwners = append(evidence.QuarantinedOwners, owner)
		evidence.Quarantined = append(evidence.Quarantined, id)
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

func canonicalVMID(name string) bool {
	return ids.Validate(name) == nil
}

func ownedVMCandidates(workDir string, jailerDir string) ([]ownerCandidate, error) {
	entries, err := custody.ReadDirectory(workDir, "vms", "guest")
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
		owner, readErr := (custody.StateRoot{Base: workDir, Components: []string{"vms", "guest"}}).ReadOwner(id)
		if readErr != nil {
			candidates = append(candidates, ownerCandidate{Owner: vm.Owner{ID: id}, Label: label, Problem: "read exact VM owner marker: " + readErr.Error()})
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
	jailerEntries, err := custody.ReadDirectory(jailerDir, "firecracker")
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

func removeOwnedRecoveryState(workDir string, owner vm.Owner) error {
	statePath := filepath.Join(workDir, "vms", "guest", owner.ID)
	recorded, err := (custody.StateRoot{Base: workDir, Components: []string{"vms", "guest"}}).ReadOwner(owner.ID)
	if err != nil {
		return fmt.Errorf("read VM owner marker before cleanup: %w", err)
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
		restoreErr := os.WriteFile(markerPath, []byte(string(owner.Kind)+"\n"+owner.ID+"\n"), 0o600)
		return errors.Join(err, restoreErr)
	}
	return nil
}
