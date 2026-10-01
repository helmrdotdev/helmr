package guestd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

var errCaptureAbortCleanupFailed = errors.New("capture abort cancellation cleanup failed")

var errCaptureAborted = errors.New("checkpoint capture aborted on its source")

type captureAbortInstallation struct {
	capture    *computerv0.FreezeComputerRequest
	version    int64
	members    map[string]*captureAbortMember
	activated  bool
	beforeSeal bool
}

type captureAbortMember struct {
	identity  *computerv0.ComputerCaptureRun
	claim     *managedProgramClaim
	slot      *waitingRunSlot
	cancelled bool
	released  bool
}

func handleComputerCaptureAbort(ctx context.Context, conn programConnection, bodyLen uint64, mounts *computerOperationRegistry, waits *waitingRunRegistry) error {
	if bodyLen != 0 || mounts == nil || waits == nil {
		return errors.New("capture abort requires registries and an empty stream body")
	}
	ctx, cancel := context.WithTimeout(ctx, resumeAttachTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := conn.SetReadDeadline(time.Now().Add(resumeAttachTimeout)); err != nil {
		return err
	}
	var request computerv0.ComputerCaptureAbortRequest
	if err := frameio.ReadProtoFrameBounded(conn, maxProgramControlFrameBytes, &request); err != nil {
		return err
	}
	err := mounts.applyCaptureAbort(ctx, waits, &request, time.Now)
	if err != nil && !errors.Is(err, errCaptureAbortCleanupFailed) {
		return err
	}
	return frameio.WriteProtoFrame(conn, &computerv0.ComputerCaptureAbortResponse{CheckpointId: request.Capture.CheckpointId, AbortDesiredVersion: request.AbortDesiredVersion, Activated: request.Activate && err == nil, CleanupFailed: err != nil})
}

// Grant installation and member release are separate so the host can restore
// ordinary renewal before any cgroup thaws. The abort receipt may advance a
// local expired grant, but never change its source, Run, attempt or lease.
func (r *computerOperationRegistry) applyCaptureAbort(ctx context.Context, waits *waitingRunRegistry, request *computerv0.ComputerCaptureAbortRequest, clock func() time.Time) error {
	r.captureAbortMu.Lock()
	defer r.captureAbortMu.Unlock()
	capture := request.GetCapture()
	if capture == nil || capture.CheckpointId == "" || capture.ComputerId == "" || capture.ComputerInstanceId == "" || capture.WriterGeneration <= 0 || capture.DesiredVersion <= 0 || capture.MembershipRevision < 0 || request.AbortDesiredVersion != capture.DesiredVersion+1 || len(request.Members) != len(capture.Runs) {
		return errors.New("capture abort identity is incomplete")
	}
	r.mu.RLock()
	var entry *computerMountEntry
	for _, candidate := range r.entries {
		if candidate != nil {
			if entry != nil && entry != candidate {
				r.mu.RUnlock()
				return errors.New("capture abort has ambiguous mounts")
			}
			entry = candidate
		}
	}
	r.mu.RUnlock()
	if entry != nil {
		entry.lifecycleMu.Lock()
		entry.finalizationMu.Lock()
	}
	installation, err := func() (*captureAbortInstallation, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		waits.mu.Lock()
		defer waits.mu.Unlock()
		if entry == nil {
			prepared := r.preparedRuntime
			if prepared == nil || prepared.computerID != capture.ComputerId || prepared.computerInstanceID != capture.ComputerInstanceId || prepared.writerGeneration != capture.WriterGeneration || len(capture.Runs) != 0 || len(r.entries) != 0 {
				return nil, errors.New("capture abort prepared source differs")
			}
		} else if r.entries[capture.ComputerInstanceId] != entry || entry.retired || entry.stopping || entry.computerID != capture.ComputerId || entry.writerGeneration != capture.WriterGeneration {
			return nil, errors.New("capture abort source differs")
		}
		if r.materializations != 0 {
			return nil, errors.New("capture abort cannot replace materialization")
		}
		if r.captureRequest != nil && !proto.Equal(r.captureRequest, capture) {
			return nil, errors.New("capture abort belongs to another seal")
		}
		installed := r.captureAbort
		if installed != nil && (!proto.Equal(installed.capture, capture) || installed.version != request.AbortDesiredVersion) {
			if err := r.validateNextCaptureLocked(capture); err != nil {
				return nil, err
			}
			installed = nil
		}
		if r.restoredMaterialization != nil {
			if err := r.validateNextCaptureLocked(capture); err != nil {
				return nil, err
			}
		}
		if request.Activate && installed == nil {
			return nil, errors.New("capture abort grants are not installed")
		}
		if installed == nil {
			installed = &captureAbortInstallation{capture: proto.Clone(capture).(*computerv0.FreezeComputerRequest), version: request.AbortDesiredVersion, members: make(map[string]*captureAbortMember), beforeSeal: r.captureRequest == nil}
		}
		expected := make(map[string]*computerv0.ComputerCaptureRun, len(capture.Runs))
		for _, m := range capture.Runs {
			if m == nil || m.RunId == "" || m.RunWaitId == "" || m.RunLeaseId == "" || m.AttemptNumber == 0 || expected[m.RunId] != nil {
				return nil, errors.New("capture abort membership is invalid")
			}
			expected[m.RunId] = m
		}
		type update struct {
			member    *captureAbortMember
			grant     *computerv0.ComputerRunAuthority
			cancelled bool
		}
		updates := make([]update, 0, len(request.Members))
		for _, m := range request.Members {
			if m == nil || m.Member == nil || !proto.Equal(expected[m.Member.RunId], m.Member) {
				return nil, errors.New("capture abort member differs")
			}
			delete(expected, m.Member.RunId)
			member := installed.members[m.Member.RunId]
			if member == nil {
				var claim *managedProgramClaim
				for _, candidate := range r.programClaims {
					if candidate != nil && candidate.entry == entry && candidate.authority.GetFence().GetRunId() == m.Member.RunId {
						claim = candidate
						break
					}
				}
				slot := waits.slots[m.Member.RunWaitId]
				if claim == nil && !m.Cancelled {
					return nil, errors.New("capture abort source claim is missing")
				}
				if claim != nil {
					f := claim.authority.GetFence()
					if f.GetComputerInstanceId() != capture.ComputerInstanceId || f.GetComputerId() != capture.ComputerId || f.GetWriterGeneration() != capture.WriterGeneration || f.GetRunLeaseId() != m.Member.RunLeaseId || f.GetAttemptNumber() != m.Member.AttemptNumber {
						return nil, errors.New("capture abort source claim changed")
					}
				} else if m.Cancelled && entry != nil {
					// Ordinary cleanup can finish before the first abort request.
					// Its tombstone retains the scoped result, not an execution grant.
					claim = entry.programCleanup[m.Member.RunLeaseId]
					if claim != nil {
						f := claim.authority.GetFence()
						if claim.entry != entry || f.GetRunId() != m.Member.RunId || f.GetRunLeaseId() != m.Member.RunLeaseId || f.GetAttemptNumber() != m.Member.AttemptNumber {
							return nil, errors.New("capture abort cleanup claim changed")
						}
					}
				}
				if slot != nil && (slot.checkpointID != capture.CheckpointId || slot.checkpointRequestVersion != capture.DesiredVersion || slot.runLeaseID != m.Member.RunLeaseId || slot.runID != m.Member.RunId || slot.attemptNumber != m.Member.AttemptNumber || slot.granted != nil || slot.accepted != nil) {
					return nil, errors.New("capture abort wait changed")
				}
				member = &captureAbortMember{identity: proto.Clone(m.Member).(*computerv0.ComputerCaptureRun), claim: claim, slot: slot}
			}
			if member.cancelled && !m.Cancelled {
				return nil, errors.New("capture abort cannot revive a cancelled member")
			}
			if m.Cancelled {
				if m.Authority != nil {
					return nil, errors.New("cancelled capture member cannot receive authority")
				}
				if member.claim != nil && member.claim.stop == nil {
					select {
					case <-member.claim.done:
						// Completed cleanup remains authoritative across lost replies.
					default:
						return nil, errors.New("cancelled capture member has no cleanup owner")
					}
				}
			} else {
				if err := validateComputerRunAuthority(entry, m.Authority, clock()); err != nil {
					return nil, err
				}
				if member.claim == nil || member.claim.stopRequested || !computerRunAuthorityEqualExceptExpiry(member.claim.authority, m.Authority) {
					return nil, errors.New("capture abort changed source grant")
				}
			}
			if request.Activate && !m.Cancelled {
				if member.slot != nil {
					original := installed.beforeSeal && m.AttachSequence == 0 && member.slot.abortSequence == 0
					if !original && (m.AttachSequence == 0 || m.AttachSequence != member.slot.abortSequence || (!member.released && member.slot.abortStream == nil)) {
						return nil, errors.New("capture abort member transport is not prepared")
					}
				} else if m.AttachSequence != 0 {
					return nil, errors.New("unpaused capture member has an attachment")
				}
			}
			updates = append(updates, update{member, m.Authority, m.Cancelled})
		}
		// Reject local admissions absent from the CP's sealed set, even after a
		// partially acknowledged freeze. No program may escape the shared barrier.
		for _, claim := range r.programClaims {
			if claim == nil || claim.authority == nil {
				return nil, errors.New("capture abort has invalid local claims")
			}
			found := false
			for _, u := range updates {
				found = found || u.member.claim == claim
			}
			if !found {
				return nil, errors.New("capture abort omits a local claim")
			}
		}
		for _, u := range updates {
			member := u.member
			if !u.cancelled && u.grant.Fence.ExpiresAtUnixNano > member.claim.authority.Fence.ExpiresAtUnixNano {
				member.claim.authority = proto.Clone(u.grant).(*computerv0.ComputerRunAuthority)
				member.claim.previousExpiry = 0
			}
			member.cancelled = u.cancelled
			installed.members[member.identity.RunId] = member
		}
		r.captureAbort = installed
		r.restoredMaterialization = nil
		r.restoreInstallation = nil
		r.restoreActivated = false
		if !installed.activated {
			r.captureRequest = proto.Clone(capture).(*computerv0.FreezeComputerRequest)
		}
		if request.Activate {
			for _, member := range installed.members {
				if member.cancelled {
					if member.claim != nil && !member.claim.stopRequested && member.claim.stop != nil {
						member.claim.stopRequested = true
						member.claim.stop()
					}
				}

			}
		}
		return installed, nil
	}()
	if entry != nil {
		entry.finalizationMu.Unlock()
		entry.lifecycleMu.Unlock()
	}
	if err != nil || !request.Activate {
		return err
	}
	for _, member := range installation.members {
		if member.cancelled {
			if member.claim == nil {
				continue
			}
			select {
			case <-member.claim.done:
				if member.claim.cleanupErr != nil {
					return errors.Join(errCaptureAbortCleanupFailed, member.claim.cleanupErr)
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	// Cancellation cleanup may acquire registry locks; join it before taking
	// the locks again and before any healthy process can observe shared state.
	r.mu.Lock()
	waits.mu.Lock()
	if r.captureAbort != installation {
		waits.mu.Unlock()
		r.mu.Unlock()
		return errors.New("capture abort owner changed")
	}
	for _, member := range installation.members {
		if !member.cancelled && !member.released {
			if member.claim == nil || member.claim.stopRequested || member.claim.authority.GetFence().GetExpiresAtUnixNano() <= clock().UnixNano() {
				waits.mu.Unlock()
				r.mu.Unlock()
				return errors.New("capture abort authority expired before activation")
			}
		}
	}
	for _, member := range installation.members {
		if !member.cancelled && !member.released && member.slot != nil {
			// Snapshot creation requires the whole-Computer seal. Only an
			// installation preceding that seal may retain an unprepared stream.
			member.slot.abortOriginalStream = installation.beforeSeal && member.slot.abortSequence == 0
			close(member.slot.abortResume)
		}
		member.released = true
	}
	waits.mu.Unlock()
	r.mu.Unlock()
	for _, member := range installation.members {
		if !member.cancelled && member.slot != nil {
			select {
			case <-member.slot.abortDone:
				if member.slot.abortErr != nil {
					return fmt.Errorf("resume captured member: %w", member.slot.abortErr)
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.captureAbort != installation {
		return errors.New("capture abort owner changed")
	}
	installation.activated = true
	r.captureRequest = nil
	return nil
}
