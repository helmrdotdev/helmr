//go:build linux

package firecracker

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	sdk "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	ops "github.com/firecracker-microvm/firecracker-go-sdk/client/operations"
	"github.com/sirupsen/logrus"
)

func TestSDKComputerDriveUsesConfiguredRequestDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &driveDeadlineClient{}
		var client *sdk.Client
		if err := withSDKClientTimeouts(30*time.Second, func() error {
			client = sdk.NewClient("unused", logrus.NewEntry(logrus.New()), false, sdk.WithOpsClient(api))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, parentLimit := range []time.Duration{time.Minute, 5 * time.Second} {
			ctx, cancel := context.WithTimeout(t.Context(), parentLimit)
			_, err := client.PutGuestDriveByID(ctx, "computer", &models.Drive{})
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			// The pinned SDK allocates half its request budget to drive attachment.
			if got, want := time.Until(api.deadline), min(15*time.Second, parentLimit); got != want {
				t.Fatalf("drive request deadline = %s, want %s", got, want)
			}
		}
	})
}

type driveDeadlineClient struct {
	ops.ClientIface
	deadline time.Time
}

func (client *driveDeadlineClient) PutGuestDriveByID(params *ops.PutGuestDriveByIDParams) (*ops.PutGuestDriveByIDNoContent, error) {
	client.deadline, _ = params.Context.Deadline()
	return &ops.PutGuestDriveByIDNoContent{}, nil
}
