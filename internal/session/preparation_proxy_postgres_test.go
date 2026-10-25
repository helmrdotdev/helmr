package session_test

import (
	"bytes"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/dispatch"

	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func protectedPreparationAllocationFixture(t *testing.T) (preparationFixture, computer.ImageBuildExecutor, *dispatch.Allocator, string) {
	t.Helper()
	f, p, _ := preparationAllocationFixture(t)
	marker, err := secretbinding.Placeholder("protected")
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE secret_bindings SET mode='protected',placeholder=$3,allowed_origins=ARRAY['https://api.example.com'] WHERE environment_id=$1 AND image_spec_id=$2 AND placement_target='TOKEN'`, f.env, p.SpecID, marker)
	a := testAllocator(t, f.pool, f.secrets)
	allocation, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := a.DeliverImageBuild(t.Context(), *f.host(), computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, InstanceID: allocation.InstanceID, Epoch: allocation.Epoch})
	if err != nil {
		t.Fatal(err)
	}
	return f, delivery.Executor, a, marker
}

func TestPreparationProxyAllocationReusesTrustAndPinsExposure(t *testing.T) {
	f, ref, a, marker := protectedPreparationAllocationFixture(t)
	capture, err := computer.CaptureImageBuildProxyTrust(t.Context(), f.pool, *f.host(), ref.InstanceID)
	if err != nil || capture.Env["TOKEN"] != marker || len(capture.Env) != 1 || len(capture.Trust.Certificate) == 0 {
		t.Fatalf("public trust capture: %+v %v", capture.Env, err)
	}
	if _, err := a.AllocateImageBuild(t.Context(), f.env, ref.ImageBuildID, f.worker); err != nil {
		t.Fatal(err)
	}
	replay, err := computer.CaptureImageBuildProxyTrust(t.Context(), f.pool, *f.host(), ref.InstanceID)
	if err != nil || !bytes.Equal(capture.Trust.Certificate, replay.Trust.Certificate) || !bytes.Equal(capture.Trust.PrivateKeyCiphertext, replay.Trust.PrivateKeyCiphertext) {
		t.Fatal("retry replaced trust")
	}
	resolve := func(origin string, markers []string) error {
		_, err := computer.CaptureImageBuildProtectedSecrets(t.Context(), f.pool, *f.host(), ref.InstanceID, origin, markers)
		return err
	}
	if err := resolve("https://api.example.com", []string{marker}); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("value available before exposure commit: %v", err)
	}
	if _, err := computer.RecordImageBuildExposure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := f.secrets.Rotate(t.Context(), audit.System("test-fixture"), f.env, f.secretID, []byte("v2"), "rotate-proxy"); err != nil {
		t.Fatal(err)
	}
	rows, err := computer.CaptureImageBuildProtectedSecrets(t.Context(), f.pool, *f.host(), ref.InstanceID, "https://api.example.com", []string{marker})
	if err != nil {
		t.Fatal(err)
	}
	values, err := f.secrets.OpenProtectedCapture(rows, []string{marker})
	if err != nil || string(values[marker]) != "v1" {
		t.Fatalf("proxy did not use pinned version: %v", err)
	}
	clear(values[marker])
	unknown, _ := secretbinding.Placeholder("protected")
	for _, selection := range []struct {
		origin  string
		markers []string
	}{
		{"https://other.example.com", []string{marker}},
		{"https://api.example.com", []string{unknown}},
		{"https://api.example.com", []string{marker, unknown}},
		{"https://api.example.com", []string{marker, marker}},
	} {
		if err := resolve(selection.origin, selection.markers); err == nil {
			t.Fatal("invalid selector or origin accepted")
		}
	}
	stale := *f.host()
	stale.HostClaimVersion++
	if _, err := computer.CaptureImageBuildProxyTrust(t.Context(), f.pool, stale, ref.InstanceID); !errors.Is(err, workergroup.ErrStaleClaims) {
		t.Fatalf("stale CA claims: %v", err)
	}
	if _, err := computer.CaptureImageBuildProtectedSecrets(t.Context(), f.pool, stale, ref.InstanceID, "https://api.example.com", []string{marker}); !errors.Is(err, workergroup.ErrStaleClaims) {
		t.Fatalf("stale value claims: %v", err)
	}
	if _, err := f.secrets.Revoke(t.Context(), audit.System("test-fixture"), f.env, f.secretID, "revoke-proxy"); err != nil {
		t.Fatal(err)
	}
	if err := resolve("https://api.example.com", []string{marker}); err == nil {
		t.Fatal("revoked Secret resolved")
	}
	// An already authorized snapshot may finish; it cannot select a new version.
	values, err = f.secrets.OpenProtectedCapture(rows, []string{marker})
	if err != nil || string(values[marker]) != "v1" {
		t.Fatalf("authorized overlap: %v", err)
	}
	clear(values[marker])
}

func TestPreparationProxyClosesAuthorityAndClearsSigner(t *testing.T) {
	for _, transition := range []string{"capture", "failure", "physical stop", "deadline", "host epoch", "revocation"} {
		t.Run(transition, func(t *testing.T) {
			f, ref, _, marker := protectedPreparationAllocationFixture(t)
			if _, err := computer.RecordImageBuildExposure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), ref); err != nil {
				t.Fatal(err)
			}
			switch transition {
			case "capture":
				broker, _ := computer.NewImageBuildKeyBroker(f.pool, preparationWrapper(t))
				key, err := broker.WriteKey(t.Context(), *f.host(), ref)
				if err != nil {
					t.Fatal(err)
				}
				clear(key.Key)
				remote, err := cas.NewFile(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				publisher, err := computer.NewImageBuildPublisher(f.pool, remote)
				if err != nil {
					t.Fatal(err)
				}
				if err := publisher.BeginCapture(t.Context(), *f.host(), ref, disk.SeedCapacity); err != nil {
					t.Fatal(err)
				}
			case "failure":
				if err := computer.FailImageBuild(t.Context(), f.pool, *f.host(), ref, "preparation_failed"); err != nil {
					t.Fatal(err)
				}
			case "physical stop":
				if err := computer.ObserveImageBuildStopped(t.Context(), f.pool, *f.host(), computer.ImageBuildIdentity{EnvironmentID: ref.EnvironmentID, ImageBuildID: ref.ImageBuildID, InstanceID: ref.InstanceID, Epoch: ref.Epoch}); err != nil {
					t.Fatal(err)
				}
			case "deadline":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET deadline_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.env, ref.ImageBuildID)
				if err := dispatch.ExpireImageBuild(t.Context(), f.pool, f.env, ref.ImageBuildID); err != nil {
					t.Fatal(err)
				}
			case "host epoch":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET current_epoch=current_epoch+1 WHERE id=$1`, f.worker)
				if err := dispatch.ExpireImageBuild(t.Context(), f.pool, f.env, ref.ImageBuildID); err != nil {
					t.Fatal(err)
				}
			case "revocation":
				if _, err := f.secrets.Revoke(t.Context(), audit.System("test-fixture"), f.env, f.secretID, "revoke-proxy"); err != nil {
					t.Fatal(err)
				}
				if err := dispatch.ReconcileImageBuildRevocation(t.Context(), f.pool, f.env, ref.ImageBuildID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := computer.CaptureImageBuildProxyTrust(t.Context(), f.pool, *f.host(), ref.InstanceID); !errors.Is(err, computer.ErrDenied) {
				t.Fatalf("closed authority leaf: %v", err)
			}
			if _, err := computer.CaptureImageBuildProtectedSecrets(t.Context(), f.pool, *f.host(), ref.InstanceID, "https://api.example.com", []string{marker}); !errors.Is(err, computer.ErrDenied) {
				t.Fatalf("closed authority value: %v", err)
			}
			var cleared, unfenced bool
			if err := f.pool.QueryRow(t.Context(), `SELECT proxy_ca_certificate IS NULL AND proxy_ca_private_key_nonce IS NULL AND proxy_ca_private_key_ciphertext IS NULL AND proxy_ca_not_after IS NULL,fenced_at IS NULL FROM image_builds WHERE environment_id=$1 AND id=$2`, f.env, ref.ImageBuildID).Scan(&cleared, &unfenced); err != nil || !cleared {
				t.Fatalf("signer retained: %v", err)
			}
			if transition != "physical stop" && !unfenced {
				t.Fatal("logical authority loss released physical custody")
			}
		})
	}
}

func TestPreparationProxyRawAllocationAndRollbackHaveNoTrust(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	if _, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker); err != nil {
		t.Fatal(err)
	}
	var empty bool
	if err := f.pool.QueryRow(t.Context(), `SELECT proxy_ca_certificate IS NULL FROM image_builds WHERE environment_id=$1 AND id=$2`, f.env, p.ID).Scan(&empty); err != nil || !empty {
		t.Fatal("raw allocation created trust")
	}
	other, attempt, allocator := preparationAllocationFixture(t)
	marker, _ := secretbinding.Placeholder("protected")
	dbtest.MustExec(t, t.Context(), other.pool, `UPDATE secret_bindings SET mode='protected',placeholder=$3,allowed_origins=ARRAY['https://api.example.com'] WHERE environment_id=$1 AND image_spec_id=$2 AND placement_target='TOKEN'`, other.env, attempt.SpecID, marker)
	if _, err := allocator.AllocateImageBuild(t.Context(), other.env, attempt.ID, other.worker); err == nil {
		t.Fatal("missing issuer accepted")
	}
	if err := other.pool.QueryRow(t.Context(), `SELECT status='queued' AND worker_host_id IS NULL AND proxy_ca_certificate IS NULL FROM image_builds WHERE environment_id=$1 AND id=$2`, other.env, attempt.ID).Scan(&empty); err != nil || !empty {
		t.Fatal("failed trust generation leaked allocation")
	}
}
