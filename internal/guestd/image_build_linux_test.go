//go:build linux

package guestd

import (
	"bytes"
	"testing"

	"github.com/helmrdotdev/helmr/internal/buildkit"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
)

func TestImageBuildFailurePreservesOutputQuotaReasonOnWire(t *testing.T) {
	var wire bytes.Buffer
	if err := writeImageBuildFailure(
		&wire,
		nil,
		&buildkit.OutputQuotaFailure{LimitBytes: imagebuild.MaxOCIArchiveBytes},
	); err != nil {
		t.Fatal(err)
	}
	raw, err := frameio.ReadMessageFrame(&wire)
	if err != nil {
		t.Fatal(err)
	}
	result, err := imagebuild.ParseGuestResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != imagebuild.GuestFailed || result.FailureReason != imagebuild.GuestFailureOutputQuota {
		t.Fatalf("result = %#v", result)
	}
}
