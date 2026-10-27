package controlplane

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerNetworkCapabilityRequiresKnownCompatibleHost(t *testing.T) {
	control := networkpolicy.Installation{
		PrivateBusinessCIDRs: []netip.Prefix{netip.MustParsePrefix("10.42.0.0/16")},
		BlockedIPv4CIDRs:     []netip.Prefix{netip.MustParsePrefix("10.9.0.0/16")},
	}
	server := &Server{networkInstallation: control}
	for _, tc := range []struct {
		name    string
		report  *workerapi.NetworkCapabilities
		allowed bool
	}{
		{"missing", nil, false},
		{"missing installation", &workerapi.NetworkCapabilities{Version: 1}, false},
		{"old version", &workerapi.NetworkCapabilities{Version: 0, Installation: control}, false},
		{"unknown version", &workerapi.NetworkCapabilities{Version: 2, Installation: control}, false},
		{"same", &workerapi.NetworkCapabilities{Version: 1, Installation: control}, true},
		{"stricter", &workerapi.NetworkCapabilities{Version: 1, Installation: networkpolicy.Installation{PrivateBusinessCIDRs: []netip.Prefix{netip.MustParsePrefix("10.42.1.0/24")}, BlockedIPv4CIDRs: control.BlockedIPv4CIDRs}}, true},
		{"wider business range", &workerapi.NetworkCapabilities{Version: 1, Installation: networkpolicy.Installation{PrivateBusinessCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}}, false},
		{"missing deny", &workerapi.NetworkCapabilities{Version: 1, Installation: networkpolicy.Installation{PrivateBusinessCIDRs: control.PrivateBusinessCIDRs}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := server.checkWorkerNetwork(tc.report)
			if tc.allowed && err != nil {
				t.Fatal(err)
			}
			if !tc.allowed && !errors.Is(err, errNetworkPolicyUnsupported) {
				t.Fatalf("expected unavailable host, got %v", err)
			}
		})
	}
}

func TestPolicyValidationCodesSurviveFinalizationWrapping(t *testing.T) {
	for _, tc := range []struct {
		cause error
		code  string
	}{
		{definition.ErrInvalidNetworkPolicy, "invalid_network_policy"},
		{definition.ErrInvalidRuntimeMcpPolicy, "invalid_runtime_mcp_policy"},
	} {
		err := fmt.Errorf("template policy: %w", tc.cause)
		if status := errorStatus(deploymentError(err)); status != http.StatusBadRequest {
			t.Fatalf("status=%d", status)
		}
		streamed := publicDeploymentFinalizeError(err)
		if streamed.Code != tc.code || streamed.Message == "" {
			t.Fatalf("streamed=%+v", streamed)
		}
	}
}

func TestExecutionNetworkBoundariesRejectMissingCapabilityBeforeAdmission(t *testing.T) {
	server := &Server{}
	cases := []struct {
		name, body string
		handler    http.HandlerFunc
	}{
		{"computer delivery", `{"kind":"computer"}`, server.workerDeliverComputerAllocation},
		{"computer source", `{"kind":"computer"}`, server.workerComputerAllocationSource},
		{"preparation delivery", `{"kind":"preparation"}`, server.workerDeliverPreparationAllocation},
		{"seed delivery", `{"kind":"seed"}`, server.workerDeliverBootSeed},
		{"preparation start", `{}`, server.workerPreparationStart},
		{"seed start", `{}`, server.workerBootSeedStart},
		{"computer renewal", `{}`, server.workerRenewAgentComputerLease},
		{"preparation renewal", `{}`, server.workerRenewPreparation},
		{"seed renewal", `{}`, server.workerRenewBootSeed},
		{"optional preparation source", `{}`, server.workerPreparationSource},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/worker", strings.NewReader(tc.body))
			tc.handler(w, r)
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"network_policy_unsupported"`) {
				t.Fatalf("expected stable unsupported policy rejection before owner admission, got %d %s", w.Code, w.Body.String())
			}
		})
	}
}
