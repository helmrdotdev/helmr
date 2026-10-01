//go:build linux

package firecracker

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/helmrdotdev/helmr/internal/firecracker/datapath"
	"github.com/helmrdotdev/helmr/internal/secretproxy"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
)

func TestWithNetworkBindingSurvivesSnapshotHandlerReplacement(t *testing.T) {
	connector := &Connector{
		cfg:      (Config{}).WithDefaults(),
		datapath: datapath.NewManager(),
	}
	logical := vm.WorkloadBinding{
		WorkerEpoch:        1,
		OwnerID:            "019c10d5-a6f7-7af1-8f5f-000000000020",
		Generation:         1,
		ComputerInstanceID: "019c10d5-a6f7-7af1-8f5f-000000000020",
		VMPlatformID:       "vm-platform",
	}
	var installed *installedNetworkBinding
	sdkMachine, err := firecracker.NewMachine(
		context.Background(),
		firecracker.Config{},
		firecracker.WithSnapshot("/tmp/mem", "/tmp/state"),
		connector.withNetworkBinding(
			workloadLaunch,
			vm.Owner{Kind: vm.OwnerInstance, ID: logical.OwnerID},
			logical,
			&installed,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !sdkMachine.Handlers.FcInit.Has("helmr.InstallNetworkBinding") {
		t.Fatal("network binding handler was not installed after snapshot handlers")
	}
}

// Runs the installed SDK handler and the real routed network path, stopping
// before any VMM launch. Requires an isolated privileged Linux environment.
func TestNetworkBindingStartupPurposePrivileged(t *testing.T) {
	if os.Getenv("HELMR_NETWORK_E2E") != "1" {
		t.Skip("set HELMR_NETWORK_E2E=1 in a disposable privileged Linux environment")
	}
	preparationFailure := errors.New("synthetic Secret preparation rejection")
	beforeVMM := errors.New("network ready; stop before VMM")
	for _, restore := range []bool{false, true} {
		name := "cold"
		if restore {
			name = "restore"
		}
		t.Run(name, func(t *testing.T) {
			for _, test := range []struct {
				name    string
				mode    launchMode
				probe   bool
				absent  bool
				failure bool
			}{
				{name: "probe", probe: true, failure: true},
				{name: "probe without callback", probe: true, absent: true},
				{name: "workload rejection", failure: true},
				{name: "workload without callback", absent: true},
				{name: "workload empty origins"},
				{name: "unknown mode cannot bypass preparation", mode: launchMode(255), failure: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					blocked := netip.MustParsePrefix("203.0.113.0/24")
					connector := &Connector{
						cfg: Config{
							StateDir:        filepath.Join(t.TempDir(), "vms"),
							NetworkLinkPool: "198.18.0.0/29", NetworkTranslationPool: "198.19.0.0/30",
							NetworkCapacity: 2, NetworkResolverIPv4: "1.1.1.1", NetworkBlockedIPv4CIDRs: []netip.Prefix{blocked},
							IPPath: "ip", NFTPath: "nft", JailerUID: 65534, JailerGID: 65534,
						},
						datapath: datapath.NewManager(),
					}
					if err := connector.datapath.VerifyKernel(); err != nil {
						t.Fatal(err)
					}
					owner := vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
					statePath, err := createOwnerStateRoot(connector.cfg.StateDir, owner)
					if err != nil {
						t.Fatal(err)
					}
					logical := vm.WorkloadBinding{WorkerEpoch: 1, OwnerID: owner.ID, Generation: 1,
						ComputerInstanceID: owner.ID, VMPlatformID: vmplatform.Contract}
					runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
					defer cancelRuntime()
					calls := 0
					if !test.absent {
						connector.cfg.PrepareSecretTransport = func(ctx context.Context, id string, prefixes []netip.Prefix) (*secretproxy.Proxy, error) {
							calls++
							if ctx != runtimeCtx || id != owner.ID || !slices.Contains(prefixes, blocked) || !slices.Contains(prefixes, netip.MustParsePrefix(connector.cfg.NetworkLinkPool)) {
								t.Error("Secret preparation lost runtime context, identity or blocked destinations")
							}
							if test.failure {
								return nil, preparationFailure
							}
							return nil, nil
						}
					}
					mode := test.mode
					if test.probe {
						mode = startupProbeLaunch
					}
					var installed *installedNetworkBinding
					defer func() {
						if installed != nil {
							if err := installed.Close(); err != nil {
								t.Error(err)
							}
						}
						if err := connector.cleanupNetworkAttachment(context.Background(), owner); err != nil {
							t.Error(err)
						}
						if err := removeStateRootLast(statePath, owner); err != nil {
							t.Error(err)
						}
						if exists, err := connector.runtimeNetNSExists(context.Background(), owner.ID); err != nil || exists {
							t.Errorf("namespace remains: exists=%t err=%v", exists, err)
						}
					}()
					opts := []firecracker.Opt{}
					if restore {
						opts = append(opts, withSnapshotRestore("unused.mem", "unused.state"))
					}
					opts = append(opts, connector.withNetworkBinding(mode, owner, logical, &installed))
					sdkMachine, err := firecracker.NewMachine(t.Context(), firecracker.Config{DisableValidation: true}, opts...)
					if err != nil {
						t.Fatal(err)
					}
					sdkMachine.Handlers.FcInit = sdkMachine.Handlers.FcInit.Swap(firecracker.Handler{
						Name: firecracker.SetupNetworkHandlerName,
						Fn:   func(context.Context, *firecracker.Machine) error { return beforeVMM },
					})
					// The installed handler runs with an independent VM lifetime context.
					err = sdkMachine.Start(runtimeCtx)
					wantCalls := 1
					if test.probe || test.absent {
						wantCalls = 0
					}
					if calls != wantCalls {
						t.Errorf("Secret preparation calls = %d, want %d", calls, wantCalls)
					}
					if !test.probe && (test.failure || test.absent) {
						if test.failure && !errors.Is(err, preparationFailure) || test.absent && (err == nil || !strings.Contains(err.Error(), "preparation is not configured")) {
							t.Errorf("preparation failure = %v", err)
						}
						if installed != nil || errors.Is(err, beforeVMM) {
							t.Error("failed preparation allowed network startup")
						}
						return
					}
					if !errors.Is(err, beforeVMM) || installed == nil {
						t.Fatalf("network did not complete before VMM: installed=%t err=%v", installed != nil, err)
					}
					if installed.secretProxy != nil || len(protectedPorts(installed.secretProxy)) != 0 {
						t.Error("unexpected protected transport")
					}
					if err := installed.verify(true); err != nil {
						t.Fatalf("routed isolation was not installed: %v", err)
					}
				})
			}
		})
	}
}
