package controlplane

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

type constructionDB struct{ db.TxDB }

type constructionKeys struct{ computer.KeyWrapper }

type constructionTelemetry struct{ telemetry.Reader }

func completeServerConfig(t *testing.T) ServerConfig {
	t.Helper()
	fencingKey, err := disk.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	credentialKey, err := auth.NewCredentialKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := newTestUploadStore(t)
	return ServerConfig{
		ComputerKeys:                   constructionKeys{},
		Log:                            discardTestLogger(),
		DB:                             routeWorkerAuthStore{},
		TX:                             constructionDB{},
		Auth:                           identity.NewAPIKeyAuthenticator(routeWorkerAuthStore{}),
		CAS:                            store,
		BundleAdmission:                bundle.Admission{Runtime: claimResponseRuntimeDescriptor()},
		PlatformStore:                  store,
		SecretDelivery:                 claimHTTPSecrets{},
		ComputerFencingKey:             fencingKey,
		TokenCredentialKey:             credentialKey,
		TelemetryReader:                constructionTelemetry{},
		AuthKey:                        testAuthRootKey(),
		WorkerHostCredentialSigningKey: testWorkerHostCredentialSigningKey(),
	}
}

func TestNewServerAcceptsCompleteConfig(t *testing.T) {
	handler, err := NewServer(completeServerConfig(t))
	if err != nil || handler == nil {
		t.Fatalf("NewServer = %v, %v", handler, err)
	}
}

func TestNewServerRequiresCAS(t *testing.T) {
	cfg := completeServerConfig(t)
	cfg.CAS = nil
	handler, err := NewServer(cfg)
	if err == nil || handler != nil || !strings.Contains(err.Error(), "CAS is required") {
		t.Fatalf("NewServer without CAS = %v, %v", handler, err)
	}
}

func TestNewServerRequiresDeploymentBundleCollaborators(t *testing.T) {
	for name, test := range map[string]struct {
		omit func(*ServerConfig)
		want string
	}{
		"platform store": {
			omit: func(cfg *ServerConfig) { cfg.PlatformStore = nil },
			want: "platform artifact store is required",
		},
		"bundle admission": {
			omit: func(cfg *ServerConfig) { cfg.BundleAdmission = bundle.Admission{} },
			want: "deployment bundle admission runtime:",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := completeServerConfig(t)
			test.omit(&cfg)
			handler, err := NewServer(cfg)
			if err == nil || handler != nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewServer without %s = %v, %v; want %q", name, handler, err, test.want)
			}
		})
	}
}

func TestNewServerValidatesWorkerHostCredentialSigningKey(t *testing.T) {
	cfg := completeServerConfig(t)
	cfg.WorkerHostCredentialSigningKey = make([]byte, workergroup.HostCredentialSigningKeySize-1)
	handler, err := NewServer(cfg)
	if err == nil || handler != nil || !strings.Contains(err.Error(), "worker host credential signing key must be exactly 32 bytes") {
		t.Fatalf("NewServer with a short worker host credential signing key = %v, %v", handler, err)
	}
}
