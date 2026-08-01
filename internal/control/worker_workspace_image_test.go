package control

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/helmrdotdev/helmr/internal/imagecache"
)

func TestWorkspaceImageTerminalReplayExactMatchesDurableReceipt(t *testing.T) {
	operationID := uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")
	buildLeaseID := uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34")
	fingerprintBytes := sha256.Sum256([]byte("request"))
	fingerprint, err := workspaceImageDigest(fingerprintBytes[:])
	if err != nil {
		t.Fatal(err)
	}
	result := imagebuild.GuestResult{
		ExecutionABI: imagebuild.ExecutionABI, Outcome: imagebuild.GuestSucceeded,
		OCIDigest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		OCISizeBytes: 4096,
	}
	receipt, err := json.Marshal(workspaceImageOperationReceipt{
		BuildLeaseID: buildLeaseID.String(), BuildLeaseGeneration: 2, DeclarationSlot: "workspace",
		OperationID: operationID.String(), AttemptID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
		RequestFingerprint:  fingerprint,
		PlanDigest:          "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ResolutionSetDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		RequestedCacheMode:  imagebuild.CachePrefer,
		Result:              result,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := workspaceImageTerminalResult(
		db.IdempotencyClaim{State: "completed", Receipt: receipt}, buildLeaseID, 2, "workspace",
		operationID, fingerprint,
		"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		imagebuild.CachePrefer,
	)
	if err != nil || got.Result != result || got.AttemptID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33" {
		t.Fatalf("terminal result = %#v, %v", got, err)
	}
	if _, err := workspaceImageTerminalResult(
		db.IdempotencyClaim{State: "completed", Receipt: receipt}, buildLeaseID, 2, "workspace",
		operationID, fingerprint,
		"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		imagebuild.CachePrefer,
	); err == nil {
		t.Fatal("terminal replay accepted a changed resolution set")
	}
}

func TestAttachWorkspaceImageCachePreservesRequestedModeAndSkipsForbiddenEnsure(t *testing.T) {
	target := imagecache.Target{
		Authority: "ghcr.io", Username: "cache-user", Ref: "ghcr.io/helmr/cache:scope",
	}
	environmentID := uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")
	t.Run("bypass", func(t *testing.T) {
		provisioner := &recordingImageCacheProvisioner{target: target}
		assignment := api.WorkerWorkspaceImageAssignment{RequestedCacheMode: imagebuild.CacheBypass}
		(&Server{cacheRepositories: provisioner}).attachWorkspaceImageCache(t.Context(), environmentID, &assignment)
		if provisioner.targetCalls != 0 || provisioner.ensureCalls != 0 || assignment.CacheTarget != nil || assignment.EffectiveCacheColdReason != "" {
			t.Fatalf("bypass provisioner = %#v, assignment = %#v", provisioner, assignment)
		}
	})
	t.Run("authority collision", func(t *testing.T) {
		provisioner := &recordingImageCacheProvisioner{target: target}
		assignment := api.WorkerWorkspaceImageAssignment{
			DeclarationSlot: "workspace", Architecture: "x86_64", CacheScope: "sha256:scope",
			RequestedCacheMode: imagebuild.CachePrefer,
			RegistryBindings:   []imagebuild.RegistryBinding{{Authority: "ghcr.io"}},
		}
		(&Server{cacheRepositories: provisioner}).attachWorkspaceImageCache(t.Context(), environmentID, &assignment)
		if provisioner.targetCalls != 1 || provisioner.ensureCalls != 0 || assignment.CacheTarget != nil ||
			assignment.EffectiveCacheColdReason != api.WorkerWorkspaceImageCacheRegistryAuthorityCollision ||
			assignment.RequestedCacheMode != imagebuild.CachePrefer {
			t.Fatalf("collision provisioner = %#v, assignment = %#v", provisioner, assignment)
		}
	})
	t.Run("terminal replay", func(t *testing.T) {
		provisioner := &recordingImageCacheProvisioner{target: target}
		assignment := api.WorkerWorkspaceImageAssignment{
			RequestedCacheMode: imagebuild.CachePrefer,
			TerminalResult: &api.WorkerWorkspaceImageTerminalResult{
				AttemptID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
			},
		}
		(&Server{cacheRepositories: provisioner}).attachWorkspaceImageCache(t.Context(), environmentID, &assignment)
		if provisioner.targetCalls != 0 || provisioner.ensureCalls != 0 || assignment.CacheTarget != nil {
			t.Fatalf("terminal replay provisioner = %#v, assignment = %#v", provisioner, assignment)
		}
	})
}

func TestAttachWorkspaceImageCacheReturnsAttemptLocalTargetOrTypedCold(t *testing.T) {
	target := imagecache.Target{
		Authority: "ghcr.io", Username: "cache-user", Ref: "ghcr.io/helmr/cache:scope",
	}
	environmentID := uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")
	for name, ensureErr := range map[string]error{"warm": nil, "cold": errors.New("quota")} {
		t.Run(name, func(t *testing.T) {
			provisioner := &recordingImageCacheProvisioner{target: target, ensureErr: ensureErr}
			assignment := api.WorkerWorkspaceImageAssignment{
				DeclarationSlot: "workspace", Architecture: "x86_64", CacheScope: "sha256:scope",
				RequestedCacheMode: imagebuild.CachePrefer, RegistryBindings: []imagebuild.RegistryBinding{},
			}
			(&Server{cacheRepositories: provisioner}).attachWorkspaceImageCache(t.Context(), environmentID, &assignment)
			if provisioner.targetCalls != 1 || provisioner.ensureCalls != 1 || assignment.RequestedCacheMode != imagebuild.CachePrefer {
				t.Fatalf("provisioner = %#v, assignment = %#v", provisioner, assignment)
			}
			if ensureErr == nil {
				if assignment.CacheTarget == nil || assignment.CacheTarget.Binding != (imagebuild.CacheBinding{
					Authority: target.Authority, Username: target.Username, Ref: target.Ref,
				}) || assignment.EffectiveCacheColdReason != "" {
					t.Fatalf("warm assignment = %#v", assignment)
				}
			} else if assignment.CacheTarget != nil || assignment.EffectiveCacheColdReason != api.WorkerWorkspaceImageCacheUnavailable {
				t.Fatalf("cold assignment = %#v", assignment)
			}
		})
	}
}

type recordingImageCacheProvisioner struct {
	target      imagecache.Target
	targetErr   error
	ensureErr   error
	targetCalls int
	ensureCalls int
}

func (p *recordingImageCacheProvisioner) Target(
	uuid.UUID,
	string,
) (imagecache.Target, error) {
	p.targetCalls++
	return p.target, p.targetErr
}

func (p *recordingImageCacheProvisioner) Ensure(context.Context, imagecache.Target) error {
	p.ensureCalls++
	return p.ensureErr
}
