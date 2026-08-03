package programbuild

import (
	"fmt"
	"testing"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
)

func TestWorkspaceImageNetworkQuotaUsesBuildResourceLimitReason(t *testing.T) {
	err := fmt.Errorf("build workspace image: %w", &guestFailure{
		Reason:  imagebuild.GuestFailureNetworkQuota,
		Message: "image-build public-egress limit was exceeded",
	})
	if got := workspaceImageFailureReason(err); got != deployment.BuildFailureNetworkLimit {
		t.Fatalf("failure reason = %q, want %q", got, deployment.BuildFailureNetworkLimit)
	}
	if got := workspaceImageFailureReason(fmt.Errorf("ordinary image failure")); got != deployment.BuildFailureWorkspaceImageFailed {
		t.Fatalf("ordinary failure reason = %q, want %q", got, deployment.BuildFailureWorkspaceImageFailed)
	}
}
