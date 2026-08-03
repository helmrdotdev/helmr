package imagebuild

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestGuestProtocolRoundTrip(t *testing.T) {
	request := validGuestRequest(t)
	raw, err := CanonicalGuestRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseGuestRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.OperationID != request.OperationID || parsed.PlanDigest != request.PlanDigest {
		t.Fatalf("parsed request = %#v", parsed)
	}

	envelope := CredentialEnvelope{
		OperationID:         request.OperationID,
		AttemptID:           request.AttemptID,
		ResolutionSetDigest: request.ResolutionSetDigest,
		RegistryCredentials: []RegistryCredentialValue{},
	}
	envelopeRaw, err := CanonicalCredentialEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(envelopeRaw, []byte(`"registryCredentials":[]`)) {
		t.Fatalf("zero-credential envelope = %s", envelopeRaw)
	}
	parsedEnvelope, err := ParseCredentialEnvelope(envelopeRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err := MatchCredentialEnvelope(request, parsedEnvelope); err != nil {
		t.Fatal(err)
	}
}

func TestGuestProtocolRejectsMismatchedCredentialEnvelope(t *testing.T) {
	request := validGuestRequest(t)
	request.Plan.Images[0].Steps[0].From.Auth = &RegistryAuth{
		Username:       "user",
		PasswordSecret: "REGISTRY_TOKEN",
	}
	request.PlanDigest, _ = Digest(request.Plan, request.Architecture)
	request.RegistryBindings = []RegistryBinding{{
		Authority:            "docker.io",
		Username:             "user",
		ResolutionID:         uuid.Must(uuid.NewV7()).String(),
		SecretID:             uuid.Must(uuid.NewV7()).String(),
		SecretVersionID:      uuid.Must(uuid.NewV7()).String(),
		RevocationGeneration: 0,
	}}
	request.ResolutionSetDigest = ResolutionSetDigest(request.RegistryBindings)
	envelope := CredentialEnvelope{
		OperationID:         request.OperationID,
		AttemptID:           request.AttemptID,
		ResolutionSetDigest: request.ResolutionSetDigest,
		RegistryCredentials: []RegistryCredentialValue{{
			Authority: "docker.io",
			Username:  "other",
			Password:  []byte("private"),
		}},
	}
	if err := ValidateGuestRequest(request); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCredentialEnvelope(envelope); err != nil {
		t.Fatal(err)
	}
	if err := MatchCredentialEnvelope(request, envelope); err == nil {
		t.Fatal("mismatched credential envelope was accepted")
	}
}

func TestCredentialEnvelopeParseFailureClearsDecodedPasswords(t *testing.T) {
	request := validGuestRequest(t)
	envelopeRaw, err := CanonicalCredentialEnvelope(CredentialEnvelope{
		OperationID:         request.OperationID,
		AttemptID:           request.AttemptID,
		ResolutionSetDigest: request.ResolutionSetDigest,
		RegistryCredentials: []RegistryCredentialValue{{
			Authority: "ghcr.io",
			Username:  "user",
			Password:  []byte("private-token"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	envelopeRaw = bytes.Replace(envelopeRaw, []byte(request.OperationID), []byte("invalid"), 1)
	var decoded CredentialEnvelope
	if err := parseCredentialEnvelope(envelopeRaw, &decoded); err == nil {
		t.Fatal("invalid envelope was accepted")
	}
	if len(decoded.RegistryCredentials) != 1 ||
		!bytes.Equal(decoded.RegistryCredentials[0].Password, make([]byte, len("private-token"))) {
		t.Fatalf("decoded password was not cleared: %#v", decoded.RegistryCredentials)
	}
}

func TestGuestProtocolMatchesAttemptLocalCacheCredential(t *testing.T) {
	request := validGuestRequest(t)
	request.RequestedCacheMode = CachePrefer
	request.CacheBinding = &CacheBinding{
		Authority: "123456789012.dkr.ecr.us-east-1.amazonaws.com",
		Username:  "AWS",
		Ref:       "123456789012.dkr.ecr.us-east-1.amazonaws.com/helmr/cache:workspace-v0",
	}
	envelope := CredentialEnvelope{
		OperationID:         request.OperationID,
		AttemptID:           request.AttemptID,
		ResolutionSetDigest: request.ResolutionSetDigest,
		RegistryCredentials: []RegistryCredentialValue{{
			Authority: request.CacheBinding.Authority,
			Username:  request.CacheBinding.Username,
			Password:  []byte("ephemeral-token"),
		}},
	}
	if err := ValidateGuestRequest(request); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCredentialEnvelope(envelope); err != nil {
		t.Fatal(err)
	}
	if err := MatchCredentialEnvelope(request, envelope); err != nil {
		t.Fatal(err)
	}
}

func TestGuestProtocolAllowsPreferToRunColdWithoutCacheBinding(t *testing.T) {
	request := validGuestRequest(t)
	request.RequestedCacheMode = CachePrefer
	if err := ValidateGuestRequest(request); err != nil {
		t.Fatal(err)
	}
}

func TestGuestProtocolRejectsCacheBindingForBypass(t *testing.T) {
	request := validGuestRequest(t)
	request.CacheBinding = &CacheBinding{
		Authority: "123456789012.dkr.ecr.us-east-1.amazonaws.com",
		Username:  "AWS",
		Ref:       "123456789012.dkr.ecr.us-east-1.amazonaws.com/helmr/cache:workspace-v0",
	}
	if err := ValidateGuestRequest(request); err == nil {
		t.Fatal("bypass request with a cache binding was accepted")
	}
}

func TestGuestProtocolRejectsCacheUserAuthorityCollision(t *testing.T) {
	request := validGuestRequest(t)
	request.Plan.Images[0].Steps[0].From = &From{
		Ref: "123456789012.dkr.ecr.us-east-1.amazonaws.com/acme/base:1",
		Auth: &RegistryAuth{
			Username:       "user",
			PasswordSecret: "REGISTRY_TOKEN",
		},
	}
	request.PlanDigest, _ = Digest(request.Plan, request.Architecture)
	request.RegistryBindings = []RegistryBinding{{
		Authority:            "123456789012.dkr.ecr.us-east-1.amazonaws.com",
		Username:             "user",
		ResolutionID:         uuid.Must(uuid.NewV7()).String(),
		SecretID:             uuid.Must(uuid.NewV7()).String(),
		SecretVersionID:      uuid.Must(uuid.NewV7()).String(),
		RevocationGeneration: 1,
	}}
	request.ResolutionSetDigest = ResolutionSetDigest(request.RegistryBindings)
	request.RequestedCacheMode = CachePrefer
	request.CacheBinding = &CacheBinding{
		Authority: "123456789012.dkr.ecr.us-east-1.amazonaws.com",
		Username:  "AWS",
		Ref:       "123456789012.dkr.ecr.us-east-1.amazonaws.com/helmr/cache:workspace-v0",
	}
	if err := ValidateGuestRequest(request); err == nil {
		t.Fatal("same-authority user and cache bindings were accepted")
	}
}

func TestGuestProtocolRequiresCanonicalTaggedCacheReference(t *testing.T) {
	for name, ref := range map[string]string{
		"implicit latest": "123456789012.dkr.ecr.us-east-1.amazonaws.com/helmr/cache",
		"short name":      "cache:workspace-v0",
		"digest":          "123456789012.dkr.ecr.us-east-1.amazonaws.com/helmr/cache@sha256:" + strings.Repeat("0", 64),
	} {
		t.Run(name, func(t *testing.T) {
			request := validGuestRequest(t)
			request.RequestedCacheMode = CachePrefer
			request.CacheBinding = &CacheBinding{
				Authority: "123456789012.dkr.ecr.us-east-1.amazonaws.com",
				Username:  "AWS",
				Ref:       ref,
			}
			if err := ValidateGuestRequest(request); err == nil {
				t.Fatal("non-canonical cache ref was accepted")
			}
		})
	}
}

func TestGuestProtocolRejectsNonCanonicalAndUnknownShape(t *testing.T) {
	raw, err := CanonicalGuestRequest(validGuestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseGuestRequest(append([]byte(" "), raw...)); err == nil {
		t.Fatal("non-canonical request was accepted")
	}
	unknown := bytes.Replace(raw, []byte(`"executionAbi":`), []byte(`"unknown":0,"executionAbi":`), 1)
	if _, err := ParseGuestRequest(unknown); err == nil {
		t.Fatal("unknown request member was accepted")
	}
}

func TestGuestProtocolBindsPathSetAndABIs(t *testing.T) {
	for name, mutate := range map[string]func(*GuestRequest){
		"execution ABI": func(request *GuestRequest) { request.ExecutionABI = "helmr.image-build.v1" },
		"LLB ABI":       func(request *GuestRequest) { request.LLBABI = "helmr.image-llb.v1" },
		"cache ABI":     func(request *GuestRequest) { request.CacheABI = "helmr.image-cache.v1" },
		"path digest": func(request *GuestRequest) {
			request.AdmittedPaths = append(request.AdmittedPaths, SourcePath{Path: "other", Kind: SourcePathFile})
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := validGuestRequest(t)
			mutate(&request)
			if err := ValidateGuestRequest(request); err == nil {
				t.Fatal("mutated request was accepted")
			}
		})
	}
}

func TestGuestProtocolRejectsArchiveEntryCountOutsidePathSet(t *testing.T) {
	request := validGuestRequest(t)
	request.SourceArchiveEntries++
	if err := ValidateGuestRequest(request); err == nil {
		t.Fatal("source archive entry count outside the admitted path set was accepted")
	}
}

func TestCanonicalRegistryAuthorityMatchesBuildKitHosts(t *testing.T) {
	for input, want := range map[string]string{
		"registry-1.docker.io": "docker.io",
		"INDEX.DOCKER.IO:443":  "docker.io",
		"ghcr.io:0443":         "ghcr.io",
		"ghcr.io:5000":         "ghcr.io:5000",
	} {
		got, err := CanonicalRegistryAuthority(input)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("CanonicalRegistryAuthority(%q) = %q, want %q", input, got, want)
		}
	}
}

func validGuestRequest(t *testing.T) GuestRequest {
	t.Helper()
	build := validBuild()
	planDigest, err := Digest(build, "x86_64")
	if err != nil {
		t.Fatal(err)
	}
	paths := []SourcePath{
		{Path: "package.json", Kind: SourcePathFile},
		{Path: "src", Kind: SourcePathDirectory},
		{Path: "src/index.ts", Kind: SourcePathFile},
	}
	digest := "sha256:" + strings.Repeat("1", 64)
	return GuestRequest{
		ExecutionABI:           ExecutionABI,
		LLBABI:                 LLBABI,
		CacheABI:               CacheABI,
		OperationID:            uuid.Must(uuid.NewV7()).String(),
		AttemptID:              uuid.Must(uuid.NewV7()).String(),
		BuildLeaseID:           uuid.Must(uuid.NewV7()).String(),
		BuildLeaseGeneration:   1,
		WorkerEpoch:            1,
		RuntimeIdentityID:      sha256sum.DigestBytes([]byte("runtime")),
		Architecture:           "x86_64",
		Plan:                   build,
		PlanDigest:             planDigest,
		SubmittedSourceDigest:  digest,
		BuildTreeDigest:        digest,
		AdmittedPaths:          paths,
		AdmittedPathSetDigest:  PathSetDigest(paths),
		SourceArchiveDigest:    digest,
		SourceArchiveSizeBytes: 1024,
		SourceArchiveEntries:   len(paths),
		ResolutionSetDigest:    ResolutionSetDigest([]RegistryBinding{}),
		RegistryBindings:       []RegistryBinding{},
		RequestedCacheMode:     CacheBypass,
	}
}
