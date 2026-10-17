package computerhost

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/smithy-go"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type deniedCheckpointPublisher struct {
	cas.ImmutableStore
	attempts int
	first    cas.Descriptor
}

func (p *deniedCheckpointPublisher) Publish(ctx context.Context, d cas.Descriptor, file *os.File) (cas.Object, error) {
	p.attempts++
	if p.attempts == 1 {
		p.first = d
		return cas.Object{}, &smithy.GenericAPIError{Code: "AccessDenied", Fault: smithy.FaultClient}
	}
	if p.attempts == 2 && p.first != d {
		return cas.Object{}, &saveProtocolError{cause: errors.New("checkpoint retry changed immutable object")}
	}
	return p.ImmutableStore.Publish(ctx, d, file)
}

func TestAgentSaveIdleCheckpointRetainsSourceAfterStorage403(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner, client, machine, cut := checkpointPipelineFixture(t, "", "")
		retained := machine.artifact.Computer.Capture
		publisher := &deniedCheckpointPublisher{ImmutableStore: owner.machines.ComputerObjects}
		owner.machines.ComputerObjects = publisher
		saves, base := agentSaveFixture(t, "")
		saves.client = &periodicAgentSaveClient{agentSaveTestClient: base}
		saves.wake = make(chan struct{})
		saves.changes = func() disk.VersionChanges { return disk.VersionChanges{} }
		calls := 0
		saves.idle = func(ctx context.Context) error {
			calls++
			return owner.captureIdle(ctx, workerapi.ComputerAllocationDelivery{ChannelCredential: "private"}, func() func() { return func() { t.Error("resumed before checkpoint preservation") } })
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if err := saves.run(ctx); !errors.Is(err, errComputerHibernated) {
			t.Fatal(err)
		}
		if calls != 1 || publisher.attempts < 2 || machine.snapshots != 1 || machine.aborts != 0 || owner.checkpointCut != retained || client.cancelled != nil {
			t.Fatal("storage403 escaped idle owner, lost custody or repeated physical cut")
		}
		for _, step := range cut.steps {
			if step == "release" {
				t.Fatal("cut released before physical cleanup")
			}
		}
		retained.Release()
	})
}

func TestCheckpointAdmissionAndSealReconcileCredential403(t *testing.T) {
	for _, phase := range []string{"begin", "seal"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				owner, client, machine, _ := checkpointPipelineFixture(t, "", phase)
				retained := machine.artifact.Computer.Capture
				client.stepErr = &httpclient.Error{StatusCode: 403}
				if err := owner.captureIdle(t.Context(), workerapi.ComputerAllocationDelivery{ChannelCredential: "private"}, func() func() { return func() {} }); !errors.Is(err, errComputerHibernated) {
					t.Fatal(err)
				}
				if machine.snapshots != 1 || owner.checkpointCut != retained {
					t.Fatal("credential response changed checkpoint identity or source")
				}
				retained.Release()
			})
		})
	}
}

func TestCheckpointStorage403StopsOnlyAtAuthorityLoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner, _, machine, cut := checkpointPipelineFixture(t, "", "")
		retained := machine.artifact.Computer.Capture
		owner.checkpointCut = retained
		ctx, cancel := context.WithCancelCause(t.Context())
		cause := errors.New("physical lease expired")
		attempts := 0
		err := owner.retryCheckpointDurability(ctx, func(context.Context) error {
			attempts++
			if attempts == 2 {
				cancel(cause)
			}
			return &smithy.GenericAPIError{Code: "AccessDenied", Fault: smithy.FaultClient}
		})
		if !errors.Is(err, cause) || attempts != 2 || owner.checkpointCut != retained || len(cut.steps) != 0 {
			t.Fatalf("authority boundary or custody changed: %v attempts=%d steps=%v", err, attempts, cut.steps)
		}
		retained.Release()
	})
}

func TestCheckpointEncryptionBoundIsTypedPermanentFailure(t *testing.T) {
	cipher, err := NewCheckpointEncryptor(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = encryptCheckpointObject(t.Context(), cipher, t.TempDir(), "checkpoint", "memory", cas.CheckpointMemoryMediaType, bytes.NewReader([]byte{1, 2}), 1)
	if !errors.Is(err, errInvalidCheckpoint) || checkpointDurabilityRetryable(err) {
		t.Fatal("invalid snapshot bound became an unbounded durability retry", err)
	}
}
