package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/substrate"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func TestPreparedRuntimePoolCheckoutRequiresMatchingRuntimeInstance(t *testing.T) {
	pool := NewPreparedRuntimePool(nil, nil, 2, nil)
	mount := api.WorkerWorkspaceMount{
		ID:                         "mat-1",
		RuntimeID:                  "runtime-1",
		DeploymentSandboxID:        "sandbox-1",
		ImageDigest:                "image-1",
		ImageFormat:                "oci-tar",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: "sandbox-1", SizeBytes: 10, MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
	}
	key := preparedRuntimeKeyFromWorkspaceMount(mount, pool.Network)
	sessionA := &fakePreparedRuntimeSession{}
	sessionB := &fakePreparedRuntimeSession{}
	pool.entries[key] = []preparedRuntimeEntry{
		{session: sessionA, runtimeInstanceID: "instance-a", runtimeEpoch: 1, instanceToken: "token-a"},
		{session: sessionB, runtimeInstanceID: "instance-b", runtimeEpoch: 2, instanceToken: "token-b"},
	}

	if session, _, _, ok := pool.Checkout(context.Background(), mount); ok || session != nil {
		t.Fatal("checkout without runtime instance id used a prepared session")
	}
	mount.RuntimeInstanceID = "instance-missing"
	mount.RuntimeEpoch = 2
	if session, _, _, ok := pool.Checkout(context.Background(), mount); ok || session != nil {
		t.Fatal("checkout with missing runtime instance id used a prepared session")
	}
	mount.RuntimeInstanceID = "instance-b"
	mount.RuntimeEpoch = 2
	session, _, instanceToken, ok := pool.Checkout(context.Background(), mount)
	if !ok || session != sessionB || instanceToken != "token-b" {
		t.Fatalf("checkout session = %v, token = %q, ok = %v; want instance-b session token-b true", session, instanceToken, ok)
	}
	if len(pool.entries[key]) != 1 || pool.entries[key][0].session != sessionA {
		t.Fatalf("remaining entries = %#v, want only instance-a", pool.entries[key])
	}
}

func TestPreparedRuntimePoolCheckoutRequiresMatchingRuntimeEpoch(t *testing.T) {
	pool := NewPreparedRuntimePool(nil, nil, 2, nil)
	mount := api.WorkerWorkspaceMount{
		ID:                         "mat-1",
		RuntimeID:                  "runtime-1",
		DeploymentSandboxID:        "sandbox-1",
		ImageDigest:                "image-1",
		ImageFormat:                "oci-tar",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: "sandbox-1", SizeBytes: 10, MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
		RuntimeInstanceID:          "instance-a",
		RuntimeEpoch:               2,
	}
	key := preparedRuntimeKeyFromWorkspaceMount(mount, pool.Network)
	staleSession := &fakePreparedRuntimeSession{}
	currentSession := &fakePreparedRuntimeSession{}
	pool.entries[key] = []preparedRuntimeEntry{
		{session: staleSession, runtimeInstanceID: "instance-a", runtimeEpoch: 1, instanceToken: "stale-token"},
		{session: currentSession, runtimeInstanceID: "instance-a", runtimeEpoch: 2, instanceToken: "current-token"},
	}

	checkedOut, _, token, ok := pool.Checkout(context.Background(), mount)
	if !ok || checkedOut != currentSession || token != "current-token" {
		t.Fatalf("checkout session=%v token=%q ok=%v, want current epoch session", checkedOut, token, ok)
	}
	if len(pool.entries[key]) != 1 || pool.entries[key][0].session != staleSession {
		t.Fatalf("remaining entries = %#v, want only stale epoch entry", pool.entries[key])
	}

	mount.RuntimeEpoch = 3
	if checkedOut, _, token, ok := pool.Checkout(context.Background(), mount); ok || checkedOut != nil || token != "" {
		t.Fatalf("checkout with unknown epoch returned session=%v token=%q ok=%v, want miss", checkedOut, token, ok)
	}
}

func TestPreparedRuntimePoolCheckoutRejectsMissingRuntimeToken(t *testing.T) {
	pool := NewPreparedRuntimePool(nil, nil, 2, nil)
	mount := api.WorkerWorkspaceMount{
		ID:                         "mat-1",
		RuntimeID:                  "runtime-1",
		DeploymentSandboxID:        "sandbox-1",
		ImageDigest:                "image-1",
		ImageFormat:                "oci-tar",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: "sandbox-1", SizeBytes: 10, MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
		RuntimeInstanceID:          "instance-a",
		RuntimeEpoch:               1,
	}
	key := preparedRuntimeKeyFromWorkspaceMount(mount, pool.Network)
	session := newFakePreparedRuntimeSession()
	pool.entries[key] = []preparedRuntimeEntry{
		{session: session, runtimeInstanceID: "instance-a", runtimeEpoch: 1},
	}

	checkedOut, _, token, ok := pool.Checkout(context.Background(), mount)
	if ok || checkedOut != nil || token != "" {
		t.Fatalf("checkout returned session=%v token=%q ok=%v, want miss", checkedOut, token, ok)
	}
	if closeCount := session.closeCountSnapshot(); closeCount != 1 {
		t.Fatalf("session close count = %d, want 1", closeCount)
	}
	if len(pool.entries[key]) != 0 {
		t.Fatalf("remaining entries = %#v, want none", pool.entries[key])
	}
}

func TestPreparedRuntimePoolStopRuntimeFromCommandClosesMatchingEpoch(t *testing.T) {
	pool := NewPreparedRuntimePool(nil, nil, 2, nil)
	instances := &fakePreparedRuntimeInstanceClient{}
	pool.RuntimeInstances = instances
	key := "runtime-key"
	sessionA := newFakePreparedRuntimeSession()
	sessionB := newFakePreparedRuntimeSession()
	pool.entries[key] = []preparedRuntimeEntry{
		{session: sessionA, runtimeInstanceID: "runtime-a", runtimeEpoch: 2, instanceToken: "token-a"},
		{session: sessionB, runtimeInstanceID: "runtime-b", runtimeEpoch: 1, instanceToken: "token-b"},
	}

	if err := pool.StopRuntimeFromCommand(context.Background(), api.WorkerCommand{
		Kind:              "runtime_stop",
		RuntimeInstanceID: "runtime-a",
		RuntimeEpoch:      1,
	}); err != nil {
		t.Fatal(err)
	}
	if sessionA.closeCount != 0 || len(instances.closed) != 0 {
		t.Fatalf("stale stop closed session count=%d closed=%+v, want no close", sessionA.closeCount, instances.closed)
	}

	if err := pool.StopRuntimeFromCommand(context.Background(), api.WorkerCommand{
		Kind:              "runtime_stop",
		RuntimeInstanceID: "runtime-a",
		RuntimeEpoch:      2,
	}); err != nil {
		t.Fatal(err)
	}
	if sessionA.closeCount != 1 {
		t.Fatalf("runtime-a close count = %d, want 1", sessionA.closeCount)
	}
	if len(instances.closed) != 1 || instances.closed[0].ID != "runtime-a" || instances.closed[0].InstanceToken != "token-a" {
		t.Fatalf("closed instances = %+v, want runtime-a token-a", instances.closed)
	}
	if pool.readyCountLocked() != 1 || pool.entries[key][0].runtimeInstanceID != "runtime-b" {
		t.Fatalf("remaining ready entries = %+v, want runtime-b only", pool.entries[key])
	}
	if err := pool.StopRuntimeFromCommand(context.Background(), api.WorkerCommand{
		Kind:              "runtime_stop",
		RuntimeInstanceID: "runtime-a",
		RuntimeEpoch:      2,
	}); err != nil {
		t.Fatal(err)
	}
	if len(instances.closed) != 1 {
		t.Fatalf("replayed stop closed instances = %+v, want no second close", instances.closed)
	}
}

func TestPreparedRuntimePoolStopRuntimeFromCommandRemovesEntryWhenClosedTransitionFails(t *testing.T) {
	pool := NewPreparedRuntimePool(nil, nil, 2, nil)
	closedErr := errors.New("closed transition failed")
	instances := &fakePreparedRuntimeInstanceClient{closedErr: closedErr}
	pool.RuntimeInstances = instances
	key := "runtime-key"
	session := newFakePreparedRuntimeSession()
	pool.entries[key] = []preparedRuntimeEntry{{
		session:           session,
		runtimeInstanceID: "runtime-a",
		runtimeEpoch:      2,
		instanceToken:     "token-a",
	}}

	err := pool.StopRuntimeFromCommand(context.Background(), api.WorkerCommand{
		Kind:              "runtime_stop",
		RuntimeInstanceID: "runtime-a",
		RuntimeEpoch:      2,
	})
	if !errors.Is(err, closedErr) {
		t.Fatalf("error = %v, want closed transition failure", err)
	}
	if pool.readyCountLocked() != 0 {
		t.Fatalf("ready entries = %d, want closed session removed from ready pool", pool.readyCountLocked())
	}
	if session.closeCount != 1 {
		t.Fatalf("close count = %d, want close attempted", session.closeCount)
	}

	instances.closedErr = nil
	if err := pool.StopRuntimeFromCommand(context.Background(), api.WorkerCommand{
		Kind:              "runtime_stop",
		RuntimeInstanceID: "runtime-a",
		RuntimeEpoch:      2,
	}); err != nil {
		t.Fatal(err)
	}
	if pool.readyCountLocked() != 0 {
		t.Fatalf("ready entries = %d, want entry to stay removed", pool.readyCountLocked())
	}
	if session.closeCount != 1 {
		t.Fatalf("close count = %d, want no replay close of removed session", session.closeCount)
	}
}

func TestPreparedRuntimePoolStopRuntimeFromCommandClaimsEntryBeforeClose(t *testing.T) {
	pool := NewPreparedRuntimePool(nil, nil, 2, nil)
	instances := &fakePreparedRuntimeInstanceClient{}
	pool.RuntimeInstances = instances
	mount := api.WorkerWorkspaceMount{
		ID:                         "mat-1",
		RuntimeInstanceID:          "runtime-a",
		RuntimeEpoch:               2,
		RuntimeID:                  "runtime-1",
		DeploymentSandboxID:        "sandbox-1",
		ImageDigest:                "image-1",
		ImageFormat:                "oci-tar",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: "sandbox-1", SizeBytes: 10, MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
	}
	key := preparedRuntimeKeyFromWorkspaceMount(mount, pool.Network)
	session := &blockingClosePreparedRuntimeSession{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	pool.entries[key] = []preparedRuntimeEntry{{
		session:           session,
		runtimeInstanceID: "runtime-a",
		runtimeEpoch:      2,
		instanceToken:     "token-a",
	}}

	errc := make(chan error, 1)
	go func() {
		errc <- pool.StopRuntimeFromCommand(context.Background(), api.WorkerCommand{
			Kind:              "runtime_stop",
			RuntimeInstanceID: "runtime-a",
			RuntimeEpoch:      2,
		})
	}()
	select {
	case <-session.entered:
	case <-time.After(time.Second):
		t.Fatal("stop command did not enter session close")
	}
	checkedOut, _, _, ok := pool.Checkout(context.Background(), mount)
	if ok || checkedOut != nil {
		t.Fatal("checkout returned a prepared runtime while stop command owned its session")
	}
	close(session.release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if len(instances.closed) != 1 || instances.closed[0].ID != "runtime-a" || instances.closed[0].InstanceToken != "token-a" {
		t.Fatalf("closed instances = %+v, want runtime-a token-a", instances.closed)
	}
}

func TestPreparedRuntimePoolFollowWarmCommandsAdvancesIgnoredCommandCursor(t *testing.T) {
	pool := NewPreparedRuntimePool(nil, nil, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var afterIDs []int64
	client := &fakeWarmCommandClient{}
	client.onFollow = func(afterID int64, handle func(api.WorkerCommand) error) error {
		afterIDs = append(afterIDs, afterID)
		switch len(afterIDs) {
		case 1:
			if err := handle(api.WorkerCommand{ID: 10, Kind: string(api.WorkerCommandKindRunResumeWait)}); err != nil {
				return err
			}
			return errors.New("disconnect")
		default:
			cancel()
			return context.Canceled
		}
	}

	err := pool.FollowWarmCommands(ctx, client, api.WorkerCapabilities{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FollowWarmCommands error = %v, want context canceled", err)
	}
	if len(afterIDs) != 2 || afterIDs[0] != 0 || afterIDs[1] != 10 {
		t.Fatalf("afterIDs = %+v, want [0 10]", afterIDs)
	}
	if len(client.accepted) != 0 || len(client.acked) != 0 {
		t.Fatalf("ignored command accepted=%+v acked=%+v, want none", client.accepted, client.acked)
	}
}

func TestPreparedRuntimePoolSizeLimitsTotalPreparedRuntimeInventory(t *testing.T) {
	body := []byte("sandbox")
	digest := sha256sum.DigestBytes(body)
	pool := NewPreparedRuntimePool(&panicMaterializingConnector{}, fakePreparedRuntimeCAS{
		object: cas.Object{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		body:   body,
	}, 1, nil)
	instances := &fakePreparedRuntimeInstanceClient{}
	pool.RuntimeInstances = instances

	existing := api.WorkerWorkspaceMount{
		ID:                         "mat-existing",
		RuntimeID:                  "runtime-1",
		DeploymentSandboxID:        "sandbox-existing",
		ImageDigest:                "image-existing",
		ImageFormat:                "oci-tar",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
	}
	existingKey := preparedRuntimeKeyFromWorkspaceMount(existing, pool.Network)
	pool.entries[existingKey] = []preparedRuntimeEntry{
		{session: &fakePreparedRuntimeSession{}, runtimeInstanceID: "instance-existing", instanceToken: "token-existing"},
	}

	payload, err := json.Marshal(api.WorkerRuntimePrepareCommand{
		DeploymentSandboxID: "sandbox-next",
		RuntimeInstance: api.WorkerRuntimeInstance{
			ID:            "instance-next",
			InstanceToken: "token-next",
		},
		Source: api.WorkerPreparedRuntimeSource{
			DeploymentSandboxID:        "sandbox-next",
			RuntimeID:                  "runtime-1",
			RootfsDigest:               "rootfs-1",
			RuntimeABI:                 "runtime-abi",
			GuestdABI:                  "guestd-abi",
			AdapterABI:                 "adapter-abi",
			ImageDigest:                "image-next",
			ImageFormat:                "oci-tar",
			WorkspaceMountPath:         "/workspace",
			SandboxImageArtifact:       api.CASObject{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
			SandboxImageArtifactFormat: "oci-tar",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pool.WarmFromCommand(context.Background(), instances, api.WorkerCapabilities{
		RuntimeID:    "runtime-1",
		RootfsDigest: "rootfs-1",
		RuntimeABI:   "runtime-abi",
	}, api.WorkerCommand{ID: 1, Kind: "runtime_prepare", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if len(instances.created) != 0 {
		t.Fatalf("created instances = %d, want 0", len(instances.created))
	}
	failed := instances.failedSnapshot()
	if len(failed) != 1 || failed[0].ID != "instance-next" {
		t.Fatalf("failed instances = %+v, want instance-next", failed)
	}
	if pool.readyCountLocked() != 1 {
		t.Fatalf("ready count = %d, want 1", pool.readyCountLocked())
	}
}

func TestPreparedRuntimePoolPrunesReadyEntryWhenReservationRenewFails(t *testing.T) {
	pool := NewPreparedRuntimePool(nil, nil, 1, nil)
	mount := api.WorkerWorkspaceMount{
		ID:                         "mat-1",
		RuntimeID:                  "runtime-1",
		DeploymentSandboxID:        "sandbox-1",
		ImageDigest:                "image-1",
		ImageFormat:                "oci-tar",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: "sandbox-1", SizeBytes: 10, MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
	}
	key := preparedRuntimeKeyFromWorkspaceMount(mount, pool.Network)
	session := &fakePreparedRuntimeSession{}
	pool.entries[key] = []preparedRuntimeEntry{
		{session: session, runtimeInstanceID: "instance-stale", instanceToken: "token-stale"},
	}
	client := &fakePreparedRuntimeInstanceClient{
		renewErr: errors.New("runtime instance not found"),
	}

	pool.pruneUnrenewableReadyEntries(context.Background(), client)

	if pool.readyCountLocked() != 0 {
		t.Fatalf("ready count = %d, want stale entry pruned", pool.readyCountLocked())
	}
	if closeCount := session.closeCountSnapshot(); closeCount != 1 {
		t.Fatalf("session close count = %d, want 1", closeCount)
	}
	if len(client.renewed) != 1 || client.renewed[0].ID != "instance-stale" {
		t.Fatalf("renewed = %+v, want instance-stale", client.renewed)
	}
}

func TestPreparedRuntimePoolPublishesLocalEntryBeforeMarkingReady(t *testing.T) {
	body := []byte("sandbox")
	digest := sha256sum.DigestBytes(body)
	clientStream, serverStream := net.Pipe()
	defer clientStream.Close()
	defer serverStream.Close()
	session := &fakePreparedRuntimeSession{
		stream: clientStream,
		wait:   make(chan error, 1),
	}
	t.Cleanup(func() {
		session.exit(context.Canceled)
	})
	source := api.WorkerPreparedRuntimeSource{
		DeploymentSandboxID:        "sandbox-1",
		RuntimeID:                  "runtime-1",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		ImageDigest:                "image-1",
		ImageFormat:                "oci-tar",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
		ReservedCpuMillis:          1000,
		ReservedMemoryMiB:          1024,
		ReservedDiskMiB:            4096,
		ReservedExecutionSlots:     1,
	}
	pool := NewPreparedRuntimePool(successfulPreparedRuntimeConnector{session: session}, fakePreparedRuntimeCAS{
		object: cas.Object{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		body:   body,
	}, 1, nil)
	const runtimeSubstrateID = "019f180a-0000-7000-8000-000000000001"
	substrateDigest := sha256sum.DigestBytes([]byte("runtime-substrate"))
	pool.Substrates = fakePreparedRuntimeSubstrateResolver{
		result: substrate.Result{
			Digest:     substrateDigest,
			Format:     substrate.Format,
			BuilderABI: substrate.BuilderABI,
			LayoutABI:  substrate.LayoutABI,
			SizeBytes:  1024,
		},
	}
	pool.RuntimeSubstrates = fakePreparedRuntimeSubstrateCatalog{
		artifact: api.WorkerRuntimeSubstrate{
			ID:                  runtimeSubstrateID,
			DeploymentSandboxID: source.DeploymentSandboxID,
			Artifact:            api.CASObject{Digest: sha256sum.DigestBytes([]byte("encrypted-substrate")), SizeBytes: 10, MediaType: cas.RuntimeSubstrateMediaType},
			SubstrateDigest:     substrateDigest,
			Format:              substrate.Format,
			BuilderABI:          substrate.BuilderABI,
			LayoutABI:           substrate.LayoutABI,
			SizeBytes:           1024,
		},
	}
	mount := preparedRuntimeWorkspaceMountFromSource(source)
	key := preparedRuntimeKeyFromWorkspaceMount(mount, pool.Network)
	instances := &fakePreparedRuntimeInstanceClient{warmSource: source}
	pool.RuntimeInstances = instances
	readyHookCalled := false
	instances.onReady = func(request api.WorkerRuntimeInstanceStateRequest) {
		readyHookCalled = true
		if request.RuntimeSubstrateID != runtimeSubstrateID {
			t.Fatalf("runtime_substrate_id = %q, want %q", request.RuntimeSubstrateID, runtimeSubstrateID)
		}
		if entries := pool.entries[key]; len(entries) != 1 || entries[0].runtimeInstanceID != "instance-1" || entries[0].instanceToken != "token-1" {
			t.Fatalf("local entries at ready transition = %+v, want instance-1 published before DB ready is visible", entries)
		}
	}
	go acknowledgePreparedRuntime(t, serverStream)
	payload, err := json.Marshal(api.WorkerRuntimePrepareCommand{
		DeploymentSandboxID: "sandbox-1",
		RuntimeInstance: api.WorkerRuntimeInstance{
			ID:            "instance-1",
			InstanceToken: "token-1",
		},
		Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pool.WarmFromCommand(context.Background(), instances, api.WorkerCapabilities{
		RuntimeID:    "runtime-1",
		RootfsDigest: "rootfs-1",
		RuntimeABI:   "runtime-abi",
	}, api.WorkerCommand{ID: 1, Kind: "runtime_prepare", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if !readyHookCalled {
		t.Fatal("MarkRuntimeInstanceReady hook was not called")
	}
	pool.mu.Lock()
	entries := pool.entries[key]
	pool.mu.Unlock()
	if len(entries) != 1 || entries[0].runtimeInstanceID != "instance-1" || entries[0].instanceToken != "token-1" {
		t.Fatalf("local entries after ready transition = %+v, want instance-1 published", entries)
	}
}

func TestPreparedRuntimePoolCheckoutWaitsForMarkReady(t *testing.T) {
	body := []byte("sandbox")
	digest := sha256sum.DigestBytes(body)
	clientStream, serverStream := net.Pipe()
	defer clientStream.Close()
	defer serverStream.Close()
	session := &fakePreparedRuntimeSession{
		stream: clientStream,
		wait:   make(chan error, 1),
	}
	t.Cleanup(func() {
		session.exit(context.Canceled)
	})
	source := api.WorkerPreparedRuntimeSource{
		DeploymentSandboxID:        "sandbox-1",
		RuntimeID:                  "runtime-1",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		ImageDigest:                "image-1",
		ImageFormat:                "oci-tar",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
		ReservedCpuMillis:          1000,
		ReservedMemoryMiB:          1024,
		ReservedDiskMiB:            4096,
		ReservedExecutionSlots:     1,
	}
	pool := NewPreparedRuntimePool(successfulPreparedRuntimeConnector{session: session}, fakePreparedRuntimeCAS{
		object: cas.Object{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		body:   body,
	}, 1, nil)
	mount := preparedRuntimeWorkspaceMountFromSource(source)
	enteredReady := make(chan struct{})
	releaseReady := make(chan struct{})
	instances := &fakePreparedRuntimeInstanceClient{warmSource: source}
	instances.onReady = func(api.WorkerRuntimeInstanceStateRequest) {
		close(enteredReady)
		<-releaseReady
	}
	pool.RuntimeInstances = instances
	go acknowledgePreparedRuntime(t, serverStream)
	payload, err := json.Marshal(api.WorkerRuntimePrepareCommand{
		DeploymentSandboxID: "sandbox-1",
		RuntimeInstance: api.WorkerRuntimeInstance{
			ID:            "instance-1",
			InstanceToken: "token-1",
		},
		Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	go func() {
		errs <- pool.WarmFromCommand(context.Background(), instances, api.WorkerCapabilities{
			RuntimeID:    "runtime-1",
			RootfsDigest: "rootfs-1",
			RuntimeABI:   "runtime-abi",
		}, api.WorkerCommand{ID: 1, Kind: "runtime_prepare", Payload: payload})
	}()
	select {
	case <-enteredReady:
	case <-time.After(time.Second):
		t.Fatal("MarkRuntimeInstanceReady was not reached")
	}
	mount.RuntimeInstanceID = "instance-1"
	mount.RuntimeEpoch = 1
	type checkoutResult struct {
		session vm.Session
		token   string
		ok      bool
	}
	checkout := make(chan checkoutResult, 1)
	go func() {
		checkedOut, _, token, ok := pool.Checkout(context.Background(), mount)
		checkout <- checkoutResult{session: checkedOut, token: token, ok: ok}
	}()
	select {
	case result := <-checkout:
		t.Fatalf("checkout while MarkReady blocked = session:%v token:%q ok:%v, want wait", result.session, result.token, result.ok)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseReady)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	result := <-checkout
	if !result.ok || result.session != session || result.token != "token-1" {
		t.Fatalf("checkout after MarkReady returned = session:%v token:%q ok:%v, want prepared session", result.session, result.token, result.ok)
	}
	if checkedOut, _, token, ok := pool.Checkout(context.Background(), mount); ok || checkedOut != nil || token != "" {
		t.Fatalf("second checkout after MarkReady returned = session:%v token:%q ok:%v, want no local ready entry", checkedOut, token, ok)
	}
}

func TestPreparedRuntimePoolCheckoutRejectsMarkReadyFailure(t *testing.T) {
	body := []byte("sandbox")
	digest := sha256sum.DigestBytes(body)
	clientStream, serverStream := net.Pipe()
	defer clientStream.Close()
	defer serverStream.Close()
	session := &fakePreparedRuntimeSession{
		stream: clientStream,
		wait:   make(chan error, 1),
	}
	t.Cleanup(func() {
		session.exit(context.Canceled)
	})
	source := api.WorkerPreparedRuntimeSource{
		DeploymentSandboxID:        "sandbox-1",
		RuntimeID:                  "runtime-1",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		ImageDigest:                "image-1",
		ImageFormat:                "oci-tar",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
		ReservedCpuMillis:          1000,
		ReservedMemoryMiB:          1024,
		ReservedDiskMiB:            4096,
		ReservedExecutionSlots:     1,
	}
	pool := NewPreparedRuntimePool(successfulPreparedRuntimeConnector{session: session}, fakePreparedRuntimeCAS{
		object: cas.Object{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		body:   body,
	}, 1, nil)
	mount := preparedRuntimeWorkspaceMountFromSource(source)
	enteredReady := make(chan struct{})
	releaseReady := make(chan struct{})
	instances := &fakePreparedRuntimeInstanceClient{warmSource: source, readyErr: errors.New("mark ready failed")}
	instances.onReady = func(api.WorkerRuntimeInstanceStateRequest) {
		close(enteredReady)
		<-releaseReady
	}
	pool.RuntimeInstances = instances
	go acknowledgePreparedRuntime(t, serverStream)
	payload, err := json.Marshal(api.WorkerRuntimePrepareCommand{
		DeploymentSandboxID: "sandbox-1",
		RuntimeInstance: api.WorkerRuntimeInstance{
			ID:            "instance-1",
			InstanceToken: "token-1",
		},
		Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	go func() {
		errs <- pool.WarmFromCommand(context.Background(), instances, api.WorkerCapabilities{
			RuntimeID:    "runtime-1",
			RootfsDigest: "rootfs-1",
			RuntimeABI:   "runtime-abi",
		}, api.WorkerCommand{ID: 1, Kind: "runtime_prepare", Payload: payload})
	}()
	select {
	case <-enteredReady:
	case <-time.After(time.Second):
		t.Fatal("MarkRuntimeInstanceReady was not reached")
	}
	mount.RuntimeInstanceID = "instance-1"
	mount.RuntimeEpoch = 1
	type checkoutResult struct {
		session vm.Session
		token   string
		ok      bool
	}
	checkout := make(chan checkoutResult, 1)
	go func() {
		checkedOut, _, token, ok := pool.Checkout(context.Background(), mount)
		checkout <- checkoutResult{session: checkedOut, token: token, ok: ok}
	}()
	select {
	case result := <-checkout:
		t.Fatalf("checkout while MarkReady blocked = session:%v token:%q ok:%v, want wait", result.session, result.token, result.ok)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseReady)
	if err := <-errs; err != nil {
		t.Fatalf("WarmFromCommand error = %v, want handled after failed transition", err)
	}
	result := <-checkout
	if result.ok || result.session != nil || result.token != "" {
		t.Fatalf("checkout after MarkReady failed = session:%v token:%q ok:%v, want miss", result.session, result.token, result.ok)
	}
	if closeCount := session.closeCountSnapshot(); closeCount > 1 {
		t.Fatalf("session close count = %d, want at most one cleanup close after ready failure", closeCount)
	}
	failed := waitPreparedRuntimeFailedCount(t, instances, 1)
	if len(failed) != 1 || failed[0].ID != "instance-1" {
		t.Fatalf("failed instances = %+v, want instance-1", failed)
	}
}

func TestPreparedRuntimePoolCheckoutReadyWaitRespectsContext(t *testing.T) {
	body := []byte("sandbox")
	digest := sha256sum.DigestBytes(body)
	clientStream, serverStream := net.Pipe()
	defer clientStream.Close()
	defer serverStream.Close()
	session := &fakePreparedRuntimeSession{
		stream: clientStream,
		wait:   make(chan error, 1),
	}
	t.Cleanup(func() {
		session.exit(context.Canceled)
	})
	source := api.WorkerPreparedRuntimeSource{
		DeploymentSandboxID:        "sandbox-1",
		RuntimeID:                  "runtime-1",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		ImageDigest:                "image-1",
		ImageFormat:                "oci-tar",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
		ReservedCpuMillis:          1000,
		ReservedMemoryMiB:          1024,
		ReservedDiskMiB:            4096,
		ReservedExecutionSlots:     1,
	}
	pool := NewPreparedRuntimePool(successfulPreparedRuntimeConnector{session: session}, fakePreparedRuntimeCAS{
		object: cas.Object{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		body:   body,
	}, 1, nil)
	mount := preparedRuntimeWorkspaceMountFromSource(source)
	enteredReady := make(chan struct{})
	releaseReady := make(chan struct{})
	instances := &fakePreparedRuntimeInstanceClient{warmSource: source}
	instances.onReady = func(api.WorkerRuntimeInstanceStateRequest) {
		close(enteredReady)
		<-releaseReady
	}
	pool.RuntimeInstances = instances
	go acknowledgePreparedRuntime(t, serverStream)
	payload, err := json.Marshal(api.WorkerRuntimePrepareCommand{
		DeploymentSandboxID: "sandbox-1",
		RuntimeInstance: api.WorkerRuntimeInstance{
			ID:            "instance-1",
			InstanceToken: "token-1",
		},
		Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	go func() {
		errs <- pool.WarmFromCommand(context.Background(), instances, api.WorkerCapabilities{
			RuntimeID:    "runtime-1",
			RootfsDigest: "rootfs-1",
			RuntimeABI:   "runtime-abi",
		}, api.WorkerCommand{ID: 1, Kind: "runtime_prepare", Payload: payload})
	}()
	select {
	case <-enteredReady:
	case <-time.After(time.Second):
		t.Fatal("MarkRuntimeInstanceReady was not reached")
	}
	mount.RuntimeInstanceID = "instance-1"
	mount.RuntimeEpoch = 1
	ctx, cancel := context.WithCancel(context.Background())
	type checkoutResult struct {
		session vm.Session
		token   string
		ok      bool
	}
	checkout := make(chan checkoutResult, 1)
	go func() {
		checkedOut, _, token, ok := pool.Checkout(ctx, mount)
		checkout <- checkoutResult{session: checkedOut, token: token, ok: ok}
	}()
	select {
	case result := <-checkout:
		t.Fatalf("checkout while MarkReady blocked = session:%v token:%q ok:%v, want wait", result.session, result.token, result.ok)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	var result checkoutResult
	select {
	case result = <-checkout:
	case <-time.After(time.Second):
		t.Fatal("checkout did not return after context cancellation")
	}
	if result.ok || result.session != nil || result.token != "" {
		t.Fatalf("checkout after context cancellation = session:%v token:%q ok:%v, want miss", result.session, result.token, result.ok)
	}
	close(releaseReady)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if session.closeCount != 0 {
		t.Fatalf("session close count = %d, want context cancellation to leave prepared runtime open", session.closeCount)
	}
	failed := instances.failedSnapshot()
	if len(failed) != 0 {
		t.Fatalf("failed instances = %+v, want checkout cancellation to preserve prepared runtime", failed)
	}
	checkedOut, _, token, ok := pool.Checkout(context.Background(), mount)
	if !ok || checkedOut != session || token != "token-1" {
		t.Fatalf("checkout after context cancellation and MarkReady = session:%v token:%q ok:%v, want prepared session", checkedOut, token, ok)
	}
}

func TestPreparedRuntimePoolCheckoutRejectsExitedSessionAfterMarkReady(t *testing.T) {
	body := []byte("sandbox")
	digest := sha256sum.DigestBytes(body)
	clientStream, serverStream := net.Pipe()
	defer clientStream.Close()
	defer serverStream.Close()
	session := &fakePreparedRuntimeSession{
		stream: clientStream,
		wait:   make(chan error, 1),
	}
	source := api.WorkerPreparedRuntimeSource{
		DeploymentSandboxID:        "sandbox-1",
		RuntimeID:                  "runtime-1",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		ImageDigest:                "image-1",
		ImageFormat:                "oci-tar",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
		ReservedCpuMillis:          1000,
		ReservedMemoryMiB:          1024,
		ReservedDiskMiB:            4096,
		ReservedExecutionSlots:     1,
	}
	pool := NewPreparedRuntimePool(successfulPreparedRuntimeConnector{session: session}, fakePreparedRuntimeCAS{
		object: cas.Object{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		body:   body,
	}, 1, nil)
	mount := preparedRuntimeWorkspaceMountFromSource(source)
	enteredReady := make(chan struct{})
	releaseReady := make(chan struct{})
	instances := &fakePreparedRuntimeInstanceClient{warmSource: source}
	instances.onReady = func(api.WorkerRuntimeInstanceStateRequest) {
		close(enteredReady)
		<-releaseReady
	}
	pool.RuntimeInstances = instances
	go acknowledgePreparedRuntime(t, serverStream)
	payload, err := json.Marshal(api.WorkerRuntimePrepareCommand{
		DeploymentSandboxID: "sandbox-1",
		RuntimeInstance: api.WorkerRuntimeInstance{
			ID:            "instance-1",
			InstanceToken: "token-1",
		},
		Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	go func() {
		errs <- pool.WarmFromCommand(context.Background(), instances, api.WorkerCapabilities{
			RuntimeID:    "runtime-1",
			RootfsDigest: "rootfs-1",
			RuntimeABI:   "runtime-abi",
		}, api.WorkerCommand{ID: 1, Kind: "runtime_prepare", Payload: payload})
	}()
	select {
	case <-enteredReady:
	case <-time.After(time.Second):
		t.Fatal("MarkRuntimeInstanceReady was not reached")
	}
	mount.RuntimeInstanceID = "instance-1"
	mount.RuntimeEpoch = 1
	type checkoutResult struct {
		session vm.Session
		token   string
		ok      bool
	}
	checkout := make(chan checkoutResult, 1)
	go func() {
		checkedOut, _, token, ok := pool.Checkout(context.Background(), mount)
		checkout <- checkoutResult{session: checkedOut, token: token, ok: ok}
	}()
	select {
	case result := <-checkout:
		t.Fatalf("checkout while MarkReady blocked = session:%v token:%q ok:%v, want wait", result.session, result.token, result.ok)
	case <-time.After(100 * time.Millisecond):
	}
	session.exit(errors.New("session exited before ready"))
	var result checkoutResult
	select {
	case result = <-checkout:
	case <-time.After(time.Second):
		t.Fatal("checkout did not return after session exit")
	}
	if result.ok || result.session != nil || result.token != "" {
		t.Fatalf("checkout after session exit = session:%v token:%q ok:%v, want miss", result.session, result.token, result.ok)
	}
	close(releaseReady)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if closeCount := session.closeCountSnapshot(); closeCount > 1 {
		t.Fatalf("session close count = %d, want at most one cleanup close after session exit", closeCount)
	}
	failed := waitPreparedRuntimeFailedCount(t, instances, 1)
	if len(failed) != 1 || failed[0].ID != "instance-1" {
		t.Fatalf("failed instances = %+v, want instance-1", failed)
	}
}

func TestPreparedRuntimePoolDoesNotPublishEntryWhenMarkReadyFails(t *testing.T) {
	body := []byte("sandbox")
	digest := sha256sum.DigestBytes(body)
	clientStream, serverStream := net.Pipe()
	defer clientStream.Close()
	defer serverStream.Close()
	session := &fakePreparedRuntimeSession{
		stream: clientStream,
		wait:   make(chan error, 1),
	}
	t.Cleanup(func() {
		session.exit(context.Canceled)
	})
	source := api.WorkerPreparedRuntimeSource{
		DeploymentSandboxID:        "sandbox-1",
		RuntimeID:                  "runtime-1",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		ImageDigest:                "image-1",
		ImageFormat:                "oci-tar",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
		ReservedCpuMillis:          1000,
		ReservedMemoryMiB:          1024,
		ReservedDiskMiB:            4096,
		ReservedExecutionSlots:     1,
	}
	pool := NewPreparedRuntimePool(successfulPreparedRuntimeConnector{session: session}, fakePreparedRuntimeCAS{
		object: cas.Object{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		body:   body,
	}, 1, nil)
	mount := preparedRuntimeWorkspaceMountFromSource(source)
	key := preparedRuntimeKeyFromWorkspaceMount(mount, pool.Network)
	readyErr := errors.New("mark ready failed")
	instances := &fakePreparedRuntimeInstanceClient{warmSource: source, readyErr: readyErr}
	pool.RuntimeInstances = instances
	go acknowledgePreparedRuntime(t, serverStream)
	payload, err := json.Marshal(api.WorkerRuntimePrepareCommand{
		DeploymentSandboxID: "sandbox-1",
		RuntimeInstance: api.WorkerRuntimeInstance{
			ID:            "instance-1",
			InstanceToken: "token-1",
		},
		Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pool.WarmFromCommand(context.Background(), instances, api.WorkerCapabilities{
		RuntimeID:    "runtime-1",
		RootfsDigest: "rootfs-1",
		RuntimeABI:   "runtime-abi",
	}, api.WorkerCommand{ID: 1, Kind: "runtime_prepare", Payload: payload})
	if err != nil {
		t.Fatalf("WarmFromCommand error = %v, want handled after failed transition", err)
	}
	pool.mu.Lock()
	entries := pool.entries[key]
	pool.mu.Unlock()
	if len(entries) != 0 {
		t.Fatalf("local entries after ready failure = %+v, want none", entries)
	}
	if session.closeCount != 1 {
		t.Fatalf("session close count = %d, want 1", session.closeCount)
	}
	failed := instances.failedSnapshot()
	if len(failed) != 1 || failed[0].ID != "instance-1" {
		t.Fatalf("failed instances = %+v, want instance-1", failed)
	}
}

func TestPreparedRuntimePoolFollowWarmCommandsAcksFailedReadyTransition(t *testing.T) {
	body := []byte("sandbox")
	digest := sha256sum.DigestBytes(body)
	clientStream, serverStream := net.Pipe()
	defer clientStream.Close()
	defer serverStream.Close()
	session := &fakePreparedRuntimeSession{
		stream: clientStream,
		wait:   make(chan error, 1),
	}
	t.Cleanup(func() {
		session.exit(context.Canceled)
	})
	source := api.WorkerPreparedRuntimeSource{
		DeploymentSandboxID:        "sandbox-1",
		RuntimeID:                  "runtime-1",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		ImageDigest:                "image-1",
		ImageFormat:                "oci-tar",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
		ReservedCpuMillis:          1000,
		ReservedMemoryMiB:          1024,
		ReservedDiskMiB:            4096,
		ReservedExecutionSlots:     1,
	}
	pool := NewPreparedRuntimePool(successfulPreparedRuntimeConnector{session: session}, fakePreparedRuntimeCAS{
		object: cas.Object{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		body:   body,
	}, 1, nil)
	payload, err := json.Marshal(api.WorkerRuntimePrepareCommand{
		DeploymentSandboxID: "sandbox-1",
		RuntimeInstance: api.WorkerRuntimeInstance{
			ID:            "instance-1",
			InstanceToken: "token-1",
		},
		Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeWarmCommandClient{}
	client.warmSource = source
	client.readyErr = errors.New("mark ready failed")
	pool.RuntimeInstances = client
	client.onFollow = func(afterID int64, handle func(api.WorkerCommand) error) error {
		if afterID > 0 {
			cancel()
			return context.Canceled
		}
		go acknowledgePreparedRuntime(t, serverStream)
		if err := handle(api.WorkerCommand{ID: 42, Kind: string(api.WorkerCommandKindRuntimePrepare), Payload: payload}); err != nil {
			return err
		}
		cancel()
		return context.Canceled
	}

	err = pool.FollowWarmCommands(ctx, client, api.WorkerCapabilities{
		RuntimeID:    "runtime-1",
		RootfsDigest: "rootfs-1",
		RuntimeABI:   "runtime-abi",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FollowWarmCommands error = %v, want context canceled after handled command", err)
	}
	if len(client.accepted) != 1 || client.accepted[0] != 42 || len(client.acked) != 1 || client.acked[0] != 42 {
		t.Fatalf("accepted=%+v acked=%+v, want command 42 accepted and acked", client.accepted, client.acked)
	}
	failed := client.failedSnapshot()
	if len(failed) != 1 || failed[0].ID != "instance-1" {
		t.Fatalf("failed instances = %+v, want instance-1", failed)
	}
}

func TestPreparedRuntimePoolMarkReadyDoesNotBlockCheckout(t *testing.T) {
	body := []byte("sandbox")
	digest := sha256sum.DigestBytes(body)
	clientStream, serverStream := net.Pipe()
	defer clientStream.Close()
	defer serverStream.Close()
	newSession := &fakePreparedRuntimeSession{
		stream: clientStream,
		wait:   make(chan error, 1),
	}
	existingSession := newFakePreparedRuntimeSession()
	t.Cleanup(func() {
		newSession.exit(context.Canceled)
		existingSession.exit(context.Canceled)
	})
	source := api.WorkerPreparedRuntimeSource{
		DeploymentSandboxID:        "sandbox-1",
		RuntimeID:                  "runtime-1",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		ImageDigest:                "image-1",
		ImageFormat:                "oci-tar",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
		ReservedCpuMillis:          1000,
		ReservedMemoryMiB:          1024,
		ReservedDiskMiB:            4096,
		ReservedExecutionSlots:     1,
	}
	pool := NewPreparedRuntimePool(successfulPreparedRuntimeConnector{session: newSession}, fakePreparedRuntimeCAS{
		object: cas.Object{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		body:   body,
	}, 2, nil)
	mount := preparedRuntimeWorkspaceMountFromSource(source)
	key := preparedRuntimeKeyFromWorkspaceMount(mount, pool.Network)
	pool.entries[key] = []preparedRuntimeEntry{{
		session:           existingSession,
		runtimeInstanceID: "instance-existing",
		runtimeEpoch:      1,
		instanceToken:     "token-existing",
		exit:              newPreparedRuntimeSignal(),
	}}
	enteredReady := make(chan struct{})
	releaseReady := make(chan struct{})
	instances := &fakePreparedRuntimeInstanceClient{warmSource: source}
	instances.onReady = func(api.WorkerRuntimeInstanceStateRequest) {
		close(enteredReady)
		<-releaseReady
	}
	pool.RuntimeInstances = instances
	go acknowledgePreparedRuntime(t, serverStream)
	payload, err := json.Marshal(api.WorkerRuntimePrepareCommand{
		DeploymentSandboxID: "sandbox-1",
		RuntimeInstance: api.WorkerRuntimeInstance{
			ID:            "instance-new",
			InstanceToken: "token-new",
		},
		Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	go func() {
		errs <- pool.WarmFromCommand(context.Background(), instances, api.WorkerCapabilities{
			RuntimeID:    "runtime-1",
			RootfsDigest: "rootfs-1",
			RuntimeABI:   "runtime-abi",
		}, api.WorkerCommand{ID: 1, Kind: "runtime_prepare", Payload: payload})
	}()
	select {
	case <-enteredReady:
	case <-time.After(time.Second):
		t.Fatal("MarkRuntimeInstanceReady was not reached")
	}
	mount.RuntimeInstanceID = "instance-existing"
	mount.RuntimeEpoch = 1
	session, _, token, ok := pool.Checkout(context.Background(), mount)
	if !ok || session != existingSession || token != "token-existing" {
		t.Fatalf("checkout while MarkReady blocked = session:%v token:%q ok:%v, want existing ready entry", session, token, ok)
	}
	close(releaseReady)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimePrepareCommandSkipsWhileForegroundActive(t *testing.T) {
	body := []byte("sandbox")
	digest := sha256sum.DigestBytes(body)
	gate := NewBackgroundWorkGate()
	endForeground := gate.BeginForeground()
	defer endForeground()
	pool := NewPreparedRuntimePool(&panicMaterializingConnector{}, fakePreparedRuntimeCAS{
		object: cas.Object{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		body:   body,
	}, 1, nil)
	pool.BackgroundGate = gate
	instances := &fakePreparedRuntimeInstanceClient{}
	payload, err := json.Marshal(api.WorkerRuntimePrepareCommand{
		DeploymentSandboxID: "sandbox-1",
		RuntimeInstance: api.WorkerRuntimeInstance{
			ID:            "instance-1",
			InstanceToken: "token-1",
		},
		Source: api.WorkerPreparedRuntimeSource{
			DeploymentSandboxID:        "sandbox-1",
			RuntimeID:                  "runtime-1",
			RootfsDigest:               "rootfs-1",
			RuntimeABI:                 "runtime-abi",
			GuestdABI:                  "guestd-abi",
			AdapterABI:                 "adapter-abi",
			ImageDigest:                "image-1",
			ImageFormat:                "oci-tar",
			WorkspaceMountPath:         "/workspace",
			SandboxImageArtifact:       api.CASObject{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
			SandboxImageArtifactFormat: "oci-tar",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pool.WarmFromCommand(context.Background(), instances, api.WorkerCapabilities{
		RuntimeID:    "runtime-1",
		RootfsDigest: "rootfs-1",
		RuntimeABI:   "runtime-abi",
	}, api.WorkerCommand{ID: 1, Kind: "runtime_prepare", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if len(instances.createdWarm) != 0 {
		t.Fatalf("created warm instances = %d, want 0 while foreground active", len(instances.createdWarm))
	}
	failed := instances.failedSnapshot()
	if len(failed) != 1 || failed[0].ID != "instance-1" {
		t.Fatalf("failed instances = %+v, want instance-1", failed)
	}
	if pool.readyCountLocked() != 0 {
		t.Fatalf("ready count = %d, want 0", pool.readyCountLocked())
	}
}

func TestPreparedRuntimePoolFailsReadyEntryWhenSessionExits(t *testing.T) {
	pool := NewPreparedRuntimePool(nil, nil, 1, nil)
	client := &fakePreparedRuntimeInstanceClient{}
	pool.RuntimeInstances = client
	mount := api.WorkerWorkspaceMount{
		ID:                         "mat-1",
		RuntimeID:                  "runtime-1",
		DeploymentSandboxID:        "sandbox-1",
		ImageDigest:                "image-1",
		ImageFormat:                "oci-tar",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: "sandbox-1", SizeBytes: 10, MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
	}
	key := preparedRuntimeKeyFromWorkspaceMount(mount, pool.Network)
	session := newFakePreparedRuntimeSession()
	entry := preparedRuntimeEntry{
		session:           session,
		runtimeInstanceID: "instance-ready",
		instanceToken:     "token-ready",
		exit:              newPreparedRuntimeSignal(),
	}
	pool.entries[key] = []preparedRuntimeEntry{entry}
	pool.mu.Lock()
	pool.monitorReadyEntryLocked(key, entry)
	pool.mu.Unlock()

	session.exit(errors.New("firecracker exited"))

	deadline := time.After(time.Second)
	for {
		ready := pool.readyCount()
		failed := client.failedSnapshot()
		if ready == 0 && len(failed) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("ready=%d failed=%d, want entry removed and instance failed", ready, len(failed))
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	failed := client.failedSnapshot()
	if failed[0].ID != "instance-ready" {
		t.Fatalf("failed instance = %s, want instance-ready", failed[0].ID)
	}
}

func TestPreparedRuntimePoolCheckoutRejectsExitedReadyEntry(t *testing.T) {
	pool := NewPreparedRuntimePool(nil, nil, 1, nil)
	client := &fakePreparedRuntimeInstanceClient{}
	pool.RuntimeInstances = client
	mount := api.WorkerWorkspaceMount{
		ID:                         "mat-1",
		RuntimeInstanceID:          "instance-ready",
		RuntimeEpoch:               1,
		RuntimeID:                  "runtime-1",
		DeploymentSandboxID:        "sandbox-1",
		ImageDigest:                "image-1",
		ImageFormat:                "oci-tar",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: "sandbox-1", SizeBytes: 10, MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
	}
	key := preparedRuntimeKeyFromWorkspaceMount(mount, pool.Network)
	session := newFakePreparedRuntimeSession()
	exit := newPreparedRuntimeSignal()
	exit.finish(errors.New("firecracker exited"))
	pool.entries[key] = []preparedRuntimeEntry{{
		session:           session,
		runtimeInstanceID: "instance-ready",
		runtimeEpoch:      1,
		instanceToken:     "token-ready",
		exit:              exit,
	}}

	checkedOut, _, _, ok := pool.Checkout(context.Background(), mount)
	if ok || checkedOut != nil {
		t.Fatal("checkout returned an exited prepared runtime")
	}
	if pool.readyCountLocked() != 0 {
		t.Fatalf("ready count = %d, want exited entry removed", pool.readyCountLocked())
	}
	failed := client.failedSnapshot()
	if len(failed) != 1 || failed[0].ID != "instance-ready" {
		t.Fatalf("failed instances = %+v, want instance-ready", failed)
	}
	if session.closeCount != 1 {
		t.Fatalf("session close count = %d, want 1", session.closeCount)
	}
}

func TestPreparedRuntimePoolCloseCancelsInFlightRefill(t *testing.T) {
	body := []byte("sandbox")
	digest := sha256sum.DigestBytes(body)
	connector := &blockingMaterializingConnector{entered: make(chan struct{})}
	instances := &fakePreparedRuntimeInstanceClient{}
	pool := NewPreparedRuntimePool(connector, fakePreparedRuntimeCAS{
		object: cas.Object{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		body:   body,
	}, 1, nil)
	pool.RuntimeInstances = instances

	pool.Refill(context.Background(), api.WorkerWorkspaceMount{
		ID:                         "mat-1",
		RuntimeInstanceToken:       "mount-token-1",
		RuntimeID:                  "runtime-1",
		DeploymentSandboxID:        "sandbox-1",
		ImageDigest:                digest,
		ImageFormat:                "oci-tar",
		RootfsDigest:               "rootfs-1",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: digest, SizeBytes: int64(len(body)), MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
	})

	select {
	case <-connector.entered:
	case <-time.After(time.Second):
		t.Fatal("refill did not enter connector mount")
	}
	done := make(chan error, 1)
	go func() {
		done <- pool.Close(context.Background())
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pool close did not wait for in-flight refill cancellation")
	}
	if connector.materializeErr == nil || !errors.Is(connector.materializeErr, context.Canceled) {
		t.Fatalf("materializeErr = %v, want context.Canceled", connector.materializeErr)
	}
	failed := instances.failedSnapshot()
	if len(failed) != 1 {
		t.Fatalf("failed instances = %d, want 1", len(failed))
	}
}

func acknowledgePreparedRuntime(t *testing.T, stream io.ReadWriteCloser) {
	t.Helper()
	header, _, err := wire.ReadStreamFrameHeader(stream)
	if err != nil {
		t.Errorf("read prepared runtime header: %v", err)
		return
	}
	if header.Type != wire.StreamTypeWorkspaceRuntimePrepare {
		t.Errorf("prepared runtime header = %+v", header)
		return
	}
	var request workspacev0.PrepareWorkspaceRuntimeRequest
	if err := frameio.ReadProtoFrame(stream, &request); err != nil {
		t.Errorf("read prepared runtime request: %v", err)
		return
	}
	imageHeader, imageSize, err := wire.ReadStreamFrameHeader(stream)
	if err != nil {
		t.Errorf("read prepared runtime image header: %v", err)
		return
	}
	if imageHeader.Type != wire.StreamTypeRunImage {
		t.Errorf("prepared runtime image header = %+v", imageHeader)
		return
	}
	if _, err := io.Copy(io.Discard, &io.LimitedReader{R: stream, N: int64(imageSize)}); err != nil {
		t.Errorf("drain prepared runtime image: %v", err)
		return
	}
	_ = frameio.WriteProtoFrame(stream, &workspacev0.PrepareWorkspaceRuntimeResponse{
		State:      "prepared",
		RuntimeKey: request.RuntimeKey,
	})
}

type fakePreparedRuntimeCAS struct {
	object cas.Object
	body   []byte
}

func (s fakePreparedRuntimeCAS) Put(context.Context, string, io.Reader) (cas.Object, error) {
	return cas.Object{}, errors.New("not implemented")
}

func (s fakePreparedRuntimeCAS) Stage(context.Context, string) (cas.Stage, error) {
	return nil, errors.New("not implemented")
}

func (s fakePreparedRuntimeCAS) Stat(context.Context, string) (cas.Object, error) {
	return s.object, nil
}

func (s fakePreparedRuntimeCAS) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.body)), nil
}

func (s fakePreparedRuntimeCAS) Delete(context.Context, string) error {
	return errors.New("not implemented")
}

type fakePreparedRuntimeSubstrateResolver struct {
	result substrate.Result
}

func (r fakePreparedRuntimeSubstrateResolver) Resolve(context.Context, string, substrate.Source) (substrate.Result, error) {
	return r.result, nil
}

type fakePreparedRuntimeSubstrateCatalog struct {
	artifact api.WorkerRuntimeSubstrate
}

func (c fakePreparedRuntimeSubstrateCatalog) LookupRuntimeSubstrate(context.Context, api.WorkerRuntimeSubstrateLookupRequest) (api.WorkerRuntimeSubstrateLookupResponse, error) {
	return api.WorkerRuntimeSubstrateLookupResponse{RuntimeSubstrate: c.artifact}, nil
}

func (c fakePreparedRuntimeSubstrateCatalog) RegisterRuntimeSubstrate(context.Context, api.WorkerRuntimeSubstrateRegisterRequest) (api.WorkerRuntimeSubstrateRegisterResponse, error) {
	return api.WorkerRuntimeSubstrateRegisterResponse{RuntimeSubstrate: c.artifact}, nil
}

type blockingMaterializingConnector struct {
	entered        chan struct{}
	materializeErr error
}

type successfulPreparedRuntimeConnector struct {
	session vm.Session
}

type panicMaterializingConnector struct{}

func (c successfulPreparedRuntimeConnector) Connect(context.Context, vm.ConnectRequest) (vm.Session, error) {
	return nil, errors.New("not implemented")
}

func (c successfulPreparedRuntimeConnector) Materialize(context.Context, vm.MaterializeRequest) (vm.Session, error) {
	return c.session, nil
}

func (c *panicMaterializingConnector) Connect(context.Context, vm.ConnectRequest) (vm.Session, error) {
	panic("Connect should not be called")
}

func (c *panicMaterializingConnector) Materialize(context.Context, vm.MaterializeRequest) (vm.Session, error) {
	panic("Materialize should not be called")
}

func (c *blockingMaterializingConnector) Connect(context.Context, vm.ConnectRequest) (vm.Session, error) {
	return nil, errors.New("not implemented")
}

func (c *blockingMaterializingConnector) Materialize(ctx context.Context, _ vm.MaterializeRequest) (vm.Session, error) {
	close(c.entered)
	<-ctx.Done()
	c.materializeErr = ctx.Err()
	return nil, ctx.Err()
}

type fakePreparedRuntimeSession struct {
	mu         sync.Mutex
	closeCount int
	wait       chan error
	stream     io.ReadWriteCloser
}

func newFakePreparedRuntimeSession() *fakePreparedRuntimeSession {
	return &fakePreparedRuntimeSession{wait: make(chan error, 1)}
}

func (s *fakePreparedRuntimeSession) Stream() io.ReadWriteCloser {
	return nil
}

func (s *fakePreparedRuntimeSession) OpenStream(context.Context) (io.ReadWriteCloser, error) {
	if s.stream != nil {
		return s.stream, nil
	}
	return nil, errors.New("not implemented")
}

func (s *fakePreparedRuntimeSession) Wait(context.Context) error {
	if s.wait != nil {
		return <-s.wait
	}
	return nil
}

func (s *fakePreparedRuntimeSession) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCount++
	if s.stream != nil {
		_ = s.stream.Close()
	}
	return nil
}

func (s *fakePreparedRuntimeSession) closeCountSnapshot() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCount
}

func (s *fakePreparedRuntimeSession) exit(err error) {
	if s.wait != nil {
		s.wait <- err
	}
}

type blockingClosePreparedRuntimeSession struct {
	entered chan struct{}
	release chan struct{}
}

func (s *blockingClosePreparedRuntimeSession) Stream() io.ReadWriteCloser {
	return nil
}

func (s *blockingClosePreparedRuntimeSession) OpenStream(context.Context) (io.ReadWriteCloser, error) {
	return nil, errors.New("not implemented")
}

func (s *blockingClosePreparedRuntimeSession) Wait(context.Context) error {
	return nil
}

func (s *blockingClosePreparedRuntimeSession) Close(context.Context) error {
	close(s.entered)
	<-s.release
	return nil
}

type fakePreparedRuntimeInstanceClient struct {
	mu          sync.Mutex
	created     []api.WorkerPreparedRuntimeInstanceCreateRequest
	createdWarm []api.WorkerRuntimePrepareInstanceCreateRequest
	warmSource  api.WorkerPreparedRuntimeSource
	renewed     []api.WorkerRuntimeInstanceRenewRequest
	ready       []api.WorkerRuntimeInstanceStateRequest
	closed      []api.WorkerRuntimeInstanceStateRequest
	failed      []api.WorkerRuntimeInstanceStateRequest
	renewErr    error
	readyErr    error
	closedErr   error
	onReady     func(api.WorkerRuntimeInstanceStateRequest)
}

type fakeWarmCommandClient struct {
	fakePreparedRuntimeInstanceClient
	onFollow func(int64, func(api.WorkerCommand) error) error
	accepted []int64
	acked    []int64
}

func (c *fakeWarmCommandClient) FollowWorkerCommands(_ context.Context, afterID int64, handle func(api.WorkerCommand) error) error {
	if c.onFollow != nil {
		return c.onFollow(afterID, handle)
	}
	return nil
}

func (c *fakeWarmCommandClient) AcceptWorkerCommand(_ context.Context, id int64) (api.WorkerCommandAcceptResponse, error) {
	c.accepted = append(c.accepted, id)
	return api.WorkerCommandAcceptResponse{ID: id}, nil
}

func (c *fakeWarmCommandClient) AcknowledgeWorkerCommand(_ context.Context, id int64) (api.WorkerCommandAckResponse, error) {
	c.acked = append(c.acked, id)
	return api.WorkerCommandAckResponse{ID: id}, nil
}

func (c *fakePreparedRuntimeInstanceClient) CreatePreparedRuntimeInstance(_ context.Context, request api.WorkerPreparedRuntimeInstanceCreateRequest) (api.WorkerPreparedRuntimeInstanceCreateResponse, error) {
	c.mu.Lock()
	c.created = append(c.created, request)
	c.mu.Unlock()
	return api.WorkerPreparedRuntimeInstanceCreateResponse{
		Instance: api.WorkerRuntimeInstance{
			ID:            request.ID,
			InstanceToken: request.InstanceToken,
		},
	}, nil
}

func (c *fakePreparedRuntimeInstanceClient) CreateRuntimePrepareInstance(_ context.Context, request api.WorkerRuntimePrepareInstanceCreateRequest) (api.WorkerRuntimePrepareInstanceCreateResponse, error) {
	c.mu.Lock()
	c.createdWarm = append(c.createdWarm, request)
	source := c.warmSource
	c.mu.Unlock()
	if strings.TrimSpace(source.DeploymentSandboxID) == "" {
		source = api.WorkerPreparedRuntimeSource{
			DeploymentSandboxID: request.DeploymentSandboxID,
			RuntimeID:           request.RuntimeID,
			RootfsDigest:        request.RootfsDigest,
			RuntimeABI:          request.RuntimeABI,
		}
	}
	return api.WorkerRuntimePrepareInstanceCreateResponse{
		Instance: api.WorkerRuntimeInstance{
			ID:            request.ID,
			InstanceToken: request.InstanceToken,
		},
		Source: source,
	}, nil
}

func (c *fakePreparedRuntimeInstanceClient) RenewRuntimeInstance(_ context.Context, request api.WorkerRuntimeInstanceRenewRequest) (api.WorkerRuntimeInstance, error) {
	c.mu.Lock()
	c.renewed = append(c.renewed, request)
	renewErr := c.renewErr
	c.mu.Unlock()
	if renewErr != nil {
		return api.WorkerRuntimeInstance{}, renewErr
	}
	return api.WorkerRuntimeInstance{ID: request.ID, InstanceToken: request.InstanceToken, State: "ready"}, nil
}

func (c *fakePreparedRuntimeInstanceClient) MarkRuntimeInstanceReady(_ context.Context, request api.WorkerRuntimeInstanceStateRequest) (api.WorkerRuntimeInstance, error) {
	c.mu.Lock()
	c.ready = append(c.ready, request)
	onReady := c.onReady
	readyErr := c.readyErr
	c.mu.Unlock()
	if onReady != nil {
		onReady(request)
	}
	if readyErr != nil {
		return api.WorkerRuntimeInstance{}, readyErr
	}
	return api.WorkerRuntimeInstance{ID: request.ID, InstanceToken: request.InstanceToken, State: "ready"}, nil
}

func (c *fakePreparedRuntimeInstanceClient) MarkRuntimeInstanceClosed(_ context.Context, request api.WorkerRuntimeInstanceStateRequest) (api.WorkerRuntimeInstance, error) {
	c.mu.Lock()
	c.closed = append(c.closed, request)
	closedErr := c.closedErr
	c.mu.Unlock()
	if closedErr != nil {
		return api.WorkerRuntimeInstance{}, closedErr
	}
	return api.WorkerRuntimeInstance{ID: request.ID, InstanceToken: request.InstanceToken, State: "closed"}, nil
}

func (c *fakePreparedRuntimeInstanceClient) MarkRuntimeInstanceFailed(_ context.Context, request api.WorkerRuntimeInstanceStateRequest) (api.WorkerRuntimeInstance, error) {
	c.mu.Lock()
	c.failed = append(c.failed, request)
	c.mu.Unlock()
	return api.WorkerRuntimeInstance{ID: request.ID, InstanceToken: request.InstanceToken, State: "failed"}, nil
}

func (c *fakePreparedRuntimeInstanceClient) failedSnapshot() []api.WorkerRuntimeInstanceStateRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]api.WorkerRuntimeInstanceStateRequest(nil), c.failed...)
}

func waitPreparedRuntimeFailedCount(t *testing.T, client *fakePreparedRuntimeInstanceClient, want int) []api.WorkerRuntimeInstanceStateRequest {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		failed := client.failedSnapshot()
		if len(failed) >= want {
			return failed
		}
		select {
		case <-deadline:
			return failed
		case <-ticker.C:
		}
	}
}

func (p *PreparedRuntimePool) readyCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.readyCountLocked()
}
