package deployment

import (
	"errors"
	"strings"
	"testing"
)

func TestRuntimeCatalogRoundTrip(t *testing.T) {
	first, second := testRuntimeCatalogDescriptors()
	raw, err := CanonicalRuntimeCatalog([]RuntimeDescriptor{first, second})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"formatVersion":0,"runtimes":[{"architecture":"x86_64","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","formatVersion":0,"mediaType":"application/vnd.helmr.runtime.v0+squashfs","runtimeApiVersion":"helmr.runtime.v0","sizeBytes":4096},{"architecture":"aarch64","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","formatVersion":0,"mediaType":"application/vnd.helmr.runtime.v0+squashfs","runtimeApiVersion":"helmr.runtime.v0","sizeBytes":4096}]}`
	if string(raw) != want {
		t.Fatalf("canonical runtime catalog = %q, want %q", raw, want)
	}

	catalog, err := ParseRuntimeCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Resolve(first.Digest); !errors.Is(err, errRuntimeCatalogUnauthenticated) {
		t.Fatalf("unverified Resolve error = %v", err)
	}
	catalog.authenticated = true
	resolved, err := catalog.Resolve(second.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != second {
		t.Fatalf("resolved descriptor = %#v, want %#v", resolved, second)
	}
	if _, err := catalog.Resolve("sha256:" + strings.Repeat("c", 64)); !errors.Is(err, ErrRuntimeNotRegistered) {
		t.Fatalf("Resolve(unregistered) error = %v", err)
	}
}

func TestRuntimeCatalogRejectsInvalidDescriptors(t *testing.T) {
	first, second := testRuntimeCatalogDescriptors()
	tests := map[string][]RuntimeDescriptor{
		"empty":        {},
		"invalid":      {{}},
		"duplicate":    {first, first},
		"out of order": {second, first},
	}
	for name, runtimes := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := CanonicalRuntimeCatalog(runtimes); err == nil {
				t.Fatal("CanonicalRuntimeCatalog returned nil error")
			}
		})
	}
}

func TestRuntimeCatalogRequiresClosedCanonicalShape(t *testing.T) {
	first, _ := testRuntimeCatalogDescriptors()
	canonical, err := CanonicalRuntimeCatalog([]RuntimeDescriptor{first})
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"noncanonical": append([]byte(" "), canonical...),
		"unknown field": append(
			canonical[:len(canonical)-1],
			[]byte(`,"unknown":true}`)...,
		),
		"missing runtimes": []byte(`{"formatVersion":0}`),
		"duplicate field": []byte(strings.Replace(
			string(canonical),
			`"formatVersion":0`,
			`"formatVersion":0,"formatVersion":0`,
			1,
		)),
		"trailing input": append(append([]byte(nil), canonical...), []byte(`{}`)...),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRuntimeCatalog(raw); err == nil {
				t.Fatal("ParseRuntimeCatalog returned nil error")
			}
		})
	}
}

func testRuntimeCatalogDescriptors() (RuntimeDescriptor, RuntimeDescriptor) {
	first := testRuntimeDescriptor()
	second := testRuntimeDescriptor()
	second.Architecture = ArchitectureAArch64
	second.Digest = "sha256:" + strings.Repeat("b", 64)
	return first, second
}
