package controlplane

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

type constructionDB struct{ db.TxDB }

type constructionKeys struct{ agent.DataKeyWrapper }

type constructionTelemetry struct{ telemetry.Reader }

func completeServerConfig(t *testing.T) ServerConfig {
	t.Helper()
	fencingKey, err := disk.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := newTestUploadStore(t)
	allocator, err := agent.NewAllocator(constructionDB{}, make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	return ServerConfig{
		Allocator:                      allocator,
		EnvironmentExecutionLimits:     org.ExecutionLimits{MaxResidentComputers: 10, MaxCPUMillis: 16000, MaxMemoryBytes: 1 << 36, MaxReservedStorageBytes: 1 << 40, MaxOutstandingAdmissions: 100, MaxCausalDepth: 8, AdmissionRatePerSecond: 10, AdmissionBurst: 20, PreparationTimeoutMS: 600000},
		ComputerKeys:                   constructionKeys{},
		Log:                            discardTestLogger(),
		DB:                             routeWorkerAuthStore{},
		TX:                             constructionDB{},
		DiagnosticDB:                   constructionDB{},
		Auth:                           identity.NewAPIKeyAuthenticator(routeWorkerAuthStore{}),
		CAS:                            store,
		BundleAdmission:                bundle.Admission{Runtime: testRuntimeDescriptor()},
		PlatformStore:                  store,
		SecretDelivery:                 emptyTestSecretDelivery{},
		ComputerFencingKey:             fencingKey,
		TelemetryReader:                constructionTelemetry{},
		DiagnosticBounds:               diagnostic.Bounds{ChunkBytes: 1024, SourceBytes: 4096, SourceRecords: 16, EnvironmentBytes: 16384, EnvironmentRecords: 64, QueueBytes: 65536, QueueRecords: 256},
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

func TestNewServerRequiresEnvironmentExecutionLimits(t *testing.T) {
	cfg := completeServerConfig(t)
	cfg.EnvironmentExecutionLimits = org.ExecutionLimits{}
	handler, err := NewServer(cfg)
	if err == nil || handler != nil || !strings.Contains(err.Error(), "environment execution limits") {
		t.Fatalf("missing policy: %v %v", handler, err)
	}
}

func TestNewServerRequiresAllocator(t *testing.T) {
	cfg := completeServerConfig(t)
	cfg.Allocator = nil
	handler, err := NewServer(cfg)
	if err == nil || handler != nil || !strings.Contains(err.Error(), "allocation owner is required") {
		t.Fatalf("NewServer without allocator = %v, %v", handler, err)
	}
}

func testRuntimeDescriptor() artifact.RuntimeDescriptor {
	return artifact.RuntimeDescriptor{Architecture: definition.ArchitectureX8664, Digest: "sha256:" + strings.Repeat("9", 64), FormatVersion: artifact.RuntimeDescriptorFormatVersion, MediaType: artifact.RuntimeArtifactMediaType, RuntimeContract: definition.RuntimeContract, SizeBytes: 4096}
}

type emptyTestSecretDelivery struct{}

func (emptyTestSecretDelivery) OpenDeliveries(uuid.UUID, []secret.DeliveryEnvelope) ([]secret.DeliveryMaterial, error) {
	return nil, nil
}

func discardTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestNewServerRequiresDiagnosticPool(t *testing.T) {
	cfg := completeServerConfig(t)
	cfg.DiagnosticDB = nil
	if _, err := NewServer(cfg); err == nil {
		t.Fatal("missing diagnostic pool accepted")
	}
}
