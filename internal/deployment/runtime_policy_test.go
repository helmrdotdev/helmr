package deployment

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestRuntimePolicyLookups(t *testing.T) {
	x86 := testRuntimeDescriptor()
	arm := testRuntimeDescriptor()
	arm.Architecture = ArchitectureAArch64
	arm.Digest = "sha256:" + strings.Repeat("b", 64)

	policy := parseRuntimePolicyDocument(t, runtimePolicyDocument{
		Current: map[string]string{
			"eu-west-1": arm.Digest,
			"us-east-1": x86.Digest,
		},
		FormatVersion: RuntimePolicyFormatVersion,
		Runtimes:      []RuntimeDescriptor{x86, arm},
	})

	current, err := policy.Current("eu-west-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current, arm) {
		t.Fatalf("Current(eu-west-1) = %#v, want %#v", current, arm)
	}
	resolved, err := policy.Resolve(x86.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolved, x86) {
		t.Fatalf("Resolve(x86) = %#v, want %#v", resolved, x86)
	}

	if _, err := policy.Current("EU-WEST-1"); !errors.Is(err, ErrRuntimeRegionNotConfigured) {
		t.Fatalf("Current(unconfigured) error = %v", err)
	}
	if _, err := policy.Resolve("sha256:" + strings.Repeat("c", 64)); !errors.Is(err, ErrRuntimeNotRegistered) {
		t.Fatalf("Resolve(unregistered) error = %v", err)
	}
}

func TestLoadRuntimePolicy(t *testing.T) {
	document := runtimePolicyDocument{
		Current:       map[string]string{"us-east-1": testRuntimeDescriptor().Digest},
		FormatVersion: RuntimePolicyFormatVersion,
		Runtimes:      []RuntimeDescriptor{testRuntimeDescriptor()},
	}
	raw := canonicalRuntimePolicyForTest(t, document)
	path := filepath.Join(t.TempDir(), "runtime-policy.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	catalog := authenticatedRuntimeCatalogForTest(t, document.Runtimes)
	policy, err := LoadRuntimePolicy(path, catalog)
	if err != nil {
		t.Fatal(err)
	}
	current, err := policy.Current("us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if current != testRuntimeDescriptor() {
		t.Fatalf("loaded current descriptor = %#v, want %#v", current, testRuntimeDescriptor())
	}
}

func TestLoadRuntimePolicyRequiresAuthenticatedExactCatalog(t *testing.T) {
	first, second := testRuntimeCatalogDescriptors()
	document := runtimePolicyDocument{
		Current:       map[string]string{"us-east-1": first.Digest},
		FormatVersion: RuntimePolicyFormatVersion,
		Runtimes:      []RuntimeDescriptor{first},
	}
	raw := canonicalRuntimePolicyForTest(t, document)
	path := filepath.Join(t.TempDir(), "runtime-policy.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	unverifiedRaw, err := CanonicalRuntimeCatalog([]RuntimeDescriptor{first})
	if err != nil {
		t.Fatal(err)
	}
	unverified, err := ParseRuntimeCatalog(unverifiedRaw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimePolicy(path, unverified); err == nil {
		t.Fatal("LoadRuntimePolicy accepted an unauthenticated catalog")
	}

	mismatch := authenticatedRuntimeCatalogForTest(t, []RuntimeDescriptor{first, second})
	if _, err := LoadRuntimePolicy(path, mismatch); err == nil {
		t.Fatal("LoadRuntimePolicy accepted a divergent catalog")
	}

	exact := authenticatedRuntimeCatalogForTest(t, []RuntimeDescriptor{first})
	if _, err := LoadRuntimePolicy(path, exact); err != nil {
		t.Fatalf("LoadRuntimePolicy rejected exact catalog: %v", err)
	}
}

func TestValidateRuntimePolicyUpgrade(t *testing.T) {
	first := testRuntimeDescriptor()
	second := testRuntimeDescriptor()
	second.Architecture = ArchitectureAArch64
	second.Digest = "sha256:" + strings.Repeat("b", 64)
	previous := parseRuntimePolicyDocument(t, runtimePolicyDocument{
		Current:       map[string]string{"us-east-1": first.Digest},
		FormatVersion: RuntimePolicyFormatVersion,
		Runtimes:      []RuntimeDescriptor{first},
	})
	next := parseRuntimePolicyDocument(t, runtimePolicyDocument{
		Current:       map[string]string{"us-east-1": second.Digest},
		FormatVersion: RuntimePolicyFormatVersion,
		Runtimes:      []RuntimeDescriptor{first, second},
	})
	if err := ValidateRuntimePolicyUpgrade(previous, next); err != nil {
		t.Fatal(err)
	}

	removed := parseRuntimePolicyDocument(t, runtimePolicyDocument{
		Current:       map[string]string{"us-east-1": second.Digest},
		FormatVersion: RuntimePolicyFormatVersion,
		Runtimes:      []RuntimeDescriptor{second},
	})
	if err := ValidateRuntimePolicyUpgrade(previous, removed); err == nil {
		t.Fatal("expected removal rejection")
	}

	mutatedDescriptor := first
	mutatedDescriptor.SizeBytes++
	mutated := &RuntimePolicy{
		current:  map[string]string{"us-east-1": first.Digest},
		runtimes: map[string]RuntimeDescriptor{first.Digest: mutatedDescriptor},
	}
	if err := ValidateRuntimePolicyUpgrade(previous, mutated); err == nil {
		t.Fatal("expected mutation rejection")
	}
	if err := ValidateRuntimePolicyUpgrade(nil, next); err == nil {
		t.Fatal("expected nil snapshot rejection")
	}
}

func TestRuntimePolicyRejectsInvalidDocuments(t *testing.T) {
	first := testRuntimeDescriptor()
	second := testRuntimeDescriptor()
	second.Architecture = ArchitectureAArch64
	second.Digest = "sha256:" + strings.Repeat("b", 64)

	tests := map[string]runtimePolicyDocument{
		"format version": {
			Current:       map[string]string{},
			FormatVersion: 1,
			Runtimes:      []RuntimeDescriptor{first},
		},
		"null current": {
			FormatVersion: RuntimePolicyFormatVersion,
			Runtimes:      []RuntimeDescriptor{first},
		},
		"empty runtimes": {
			Current:       map[string]string{},
			FormatVersion: RuntimePolicyFormatVersion,
			Runtimes:      []RuntimeDescriptor{},
		},
		"invalid descriptor": {
			Current:       map[string]string{},
			FormatVersion: RuntimePolicyFormatVersion,
			Runtimes:      []RuntimeDescriptor{{}},
		},
		"duplicate digest": {
			Current:       map[string]string{},
			FormatVersion: RuntimePolicyFormatVersion,
			Runtimes:      []RuntimeDescriptor{first, first},
		},
		"unsorted runtimes": {
			Current:       map[string]string{},
			FormatVersion: RuntimePolicyFormatVersion,
			Runtimes:      []RuntimeDescriptor{second, first},
		},
		"empty region": {
			Current:       map[string]string{"": first.Digest},
			FormatVersion: RuntimePolicyFormatVersion,
			Runtimes:      []RuntimeDescriptor{first},
		},
		"oversized region": {
			Current:       map[string]string{strings.Repeat("r", 256): first.Digest},
			FormatVersion: RuntimePolicyFormatVersion,
			Runtimes:      []RuntimeDescriptor{first},
		},
		"unnormalized region": {
			Current:       map[string]string{" region": first.Digest},
			FormatVersion: RuntimePolicyFormatVersion,
			Runtimes:      []RuntimeDescriptor{first},
		},
		"control region": {
			Current:       map[string]string{"region\n": first.Digest},
			FormatVersion: RuntimePolicyFormatVersion,
			Runtimes:      []RuntimeDescriptor{first},
		},
		"dangling current": {
			Current:       map[string]string{"us-east-1": second.Digest},
			FormatVersion: RuntimePolicyFormatVersion,
			Runtimes:      []RuntimeDescriptor{first},
		},
	}

	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRuntimePolicy(canonicalRuntimePolicyForTest(t, document)); err == nil {
				t.Fatal("ParseRuntimePolicy returned nil error")
			}
		})
	}
}

func TestRuntimePolicyAcceptsMaximumOpaqueRegionID(t *testing.T) {
	regionID := strings.Repeat("r", 255)
	policy := parseRuntimePolicyDocument(t, runtimePolicyDocument{
		Current:       map[string]string{regionID: testRuntimeDescriptor().Digest},
		FormatVersion: RuntimePolicyFormatVersion,
		Runtimes:      []RuntimeDescriptor{testRuntimeDescriptor()},
	})
	if _, err := policy.Current(regionID); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimePolicyRequiresClosedCanonicalShape(t *testing.T) {
	document := runtimePolicyDocument{
		Current:       map[string]string{"us-east-1": testRuntimeDescriptor().Digest},
		FormatVersion: RuntimePolicyFormatVersion,
		Runtimes:      []RuntimeDescriptor{testRuntimeDescriptor()},
	}
	canonical := canonicalRuntimePolicyForTest(t, document)
	tests := map[string][]byte{
		"noncanonical": append([]byte(" "), canonical...),
		"unknown field": append(
			canonical[:len(canonical)-1],
			[]byte(`,"unknown":true}`)...,
		),
		"missing current": []byte(strings.Replace(
			string(canonical),
			`"current":{"us-east-1":"`+testRuntimeDescriptor().Digest+`"},`,
			"",
			1,
		)),
		"duplicate field": []byte(strings.Replace(
			string(canonical),
			`"formatVersion":0`,
			`"formatVersion":0,"formatVersion":0`,
			1,
		)),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRuntimePolicy(raw); err == nil {
				t.Fatal("ParseRuntimePolicy returned nil error")
			}
		})
	}
}

func parseRuntimePolicyDocument(t *testing.T, document runtimePolicyDocument) *RuntimePolicy {
	t.Helper()
	policy, err := ParseRuntimePolicy(canonicalRuntimePolicyForTest(t, document))
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func canonicalRuntimePolicyForTest(t *testing.T, document runtimePolicyDocument) []byte {
	t.Helper()
	raw, err := canonicalRuntimePolicyDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func authenticatedRuntimeCatalogForTest(
	t *testing.T,
	runtimes []RuntimeDescriptor,
) *RuntimeCatalog {
	t.Helper()
	raw, err := CanonicalRuntimeCatalog(runtimes)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := ParseRuntimeCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	catalog.authenticated = true
	return catalog
}
