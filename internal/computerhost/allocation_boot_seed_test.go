package computerhost

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type seedRenewalClient struct{ reclaims, renewals int }

func (c *seedRenewalClient) ReclaimBootSeed(context.Context, workerapi.BootSeedExecutor) error {
	c.reclaims++
	if c.reclaims == 1 {
		return &httpclient.Error{StatusCode: http.StatusForbidden}
	}
	return nil
}
func (c *seedRenewalClient) RenewBootSeed(context.Context, workerapi.BootSeedExecutor) (workerapi.PreparationRenewal, error) {
	c.renewals++
	return workerapi.PreparationRenewal{ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (*seedRenewalClient) DeliverBootSeedAllocation(context.Context, workerapi.AllocationIdentity) (workerapi.BootSeedAllocationDelivery, error) {
	panic("unexpected delivery")
}
func (*seedRenewalClient) BootSeedStart(context.Context, workerapi.BootSeedExecutor) (workerapi.ComputerAllocationSource, error) {
	panic("unexpected start")
}
func (*seedRenewalClient) PublishBootSeed(context.Context, workerapi.BootSeedPublication) error {
	panic("unexpected publication")
}
func (*seedRenewalClient) FailBootSeed(context.Context, workerapi.BootSeedFailure) error {
	panic("reclaim wait must not fail the generation")
}
func (*seedRenewalClient) ObserveBootSeedStopped(context.Context, workerapi.AllocationIdentity) error {
	panic("unexpected stop")
}

func TestOptionalSeedReclaimWaitsForPressureAcknowledgment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, policy, shape := optionalMachines(t)
		policy.Optional.MaxTasks = 0
		p.ApplyResourcePolicy(policy)
		identity := workerapi.AllocationIdentity{Kind: "seed", AllocationID: "seed", Epoch: 1}
		if err := p.reserveOptionalSeed(instanceReservationKey(identity.AllocationID, identity.Epoch), shape); err == nil {
			t.Fatal("zero tasks admitted optional work")
		}
		client := &seedRenewalClient{}
		owner := &BootSeedAllocationOwner{machines: p, client: client, identity: identity}
		owner.expires.Store(time.Now().Add(time.Minute).UnixNano())
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(context.Canceled)
		owner.renew(ctx, cancel)
		if !errors.Is(context.Cause(ctx), errOptionalReclaimed) || client.reclaims != 2 || client.renewals != 1 {
			t.Fatalf("reclaim wait: cause=%v requests=%d renewals=%d", context.Cause(ctx), client.reclaims, client.renewals)
		}
	})
}
