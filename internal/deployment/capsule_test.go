package deployment

import (
	"bytes"
	"strings"
	"testing"
)

func TestManagerAuthorityDocumentsRoundTrip(t *testing.T) {
	capsule := managerCapsuleFixture(PackageManagerBun, ArchitectureAArch64)
	capsuleRaw, err := CanonicalManagerCapsule(capsule)
	if err != nil {
		t.Fatal(err)
	}
	parsedCapsule, err := ParseManagerCapsule(capsuleRaw)
	if err != nil {
		t.Fatal(err)
	}
	if parsedCapsule != capsule {
		t.Fatalf("parsed capsule = %#v, want %#v", parsedCapsule, capsule)
	}

	capsuleDigest, err := ManagerCapsuleDigest(capsule)
	if err != nil {
		t.Fatal(err)
	}
	selector := NewManagerSelector(capsule.PackageManager, capsule.Architecture)
	selectorRaw, err := CanonicalManagerSelector(selector)
	if err != nil {
		t.Fatal(err)
	}
	parsedSelector, err := ParseManagerSelector(selectorRaw)
	if err != nil {
		t.Fatal(err)
	}
	if parsedSelector != selector {
		t.Fatalf("parsed selector = %#v, want %#v", parsedSelector, selector)
	}

	claim := ManagerClaim{
		Architecture:         capsule.Architecture,
		FormatVersion:        ManagerSelectorFormatVersion,
		ManagerCapsuleDigest: capsuleDigest,
		PackageManager:       capsule.PackageManager,
	}
	claimRaw, err := CanonicalManagerClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	parsedClaim, err := ParseManagerClaim(claimRaw)
	if err != nil {
		t.Fatal(err)
	}
	if parsedClaim != claim {
		t.Fatalf("parsed claim = %#v, want %#v", parsedClaim, claim)
	}
	if err := ValidateManagerAuthority(selector, claim, capsule); err != nil {
		t.Fatal(err)
	}
}

func TestManagerAuthorityHasStableCanonicalIdentity(t *testing.T) {
	capsule := managerCapsuleFixture(PackageManagerBun, ArchitectureAArch64)
	selector := NewManagerSelector(capsule.PackageManager, capsule.Architecture)

	selectorRaw, err := CanonicalManagerSelector(selector)
	if err != nil {
		t.Fatal(err)
	}
	const wantSelector = `{"architecture":"aarch64","formatVersion":0,"packageManager":{"name":"bun","version":"1.3.10"},"profile":"helmr.official-managers.v0"}`
	if !bytes.Equal(selectorRaw, []byte(wantSelector)) {
		t.Fatalf("selector = %s, want %s", selectorRaw, wantSelector)
	}
	selectorDigest, err := ManagerSelectorDigest(selector)
	if err != nil {
		t.Fatal(err)
	}
	const wantSelectorDigest = "sha256:a3d6175aec15e208c6d7185ded193bee71a6d83c946a8b2bf4ad455d04338b82"
	if selectorDigest != wantSelectorDigest {
		t.Fatalf("selector digest = %q, want %q", selectorDigest, wantSelectorDigest)
	}
	selectorKey, err := ManagerSelectorKey(selector)
	if err != nil {
		t.Fatal(err)
	}
	if selectorKey != "v0/claims/sha256/"+strings.TrimPrefix(
		wantSelectorDigest,
		"sha256:",
	) {
		t.Fatalf("selector key = %q", selectorKey)
	}

	capsuleRaw, err := CanonicalManagerCapsule(capsule)
	if err != nil {
		t.Fatal(err)
	}
	const wantCapsule = `{"architecture":"aarch64","entrypoint":{"kind":"native","path":"/opt/helmr/manager/bin/bun"},"formatVersion":0,"packageManager":{"name":"bun","version":"1.3.10"},"source":{"digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","origin":"https://github.com/oven-sh/bun/releases/download/bun-v1.3.10/bun-linux-aarch64.zip","sizeBytes":1},"tree":{"digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222","mediaType":"application/vnd.helmr.package-manager.v0+squashfs","sizeBytes":1}}`
	if !bytes.Equal(capsuleRaw, []byte(wantCapsule)) {
		t.Fatalf("capsule = %s, want %s", capsuleRaw, wantCapsule)
	}
	capsuleDigest, err := ManagerCapsuleDigest(capsule)
	if err != nil {
		t.Fatal(err)
	}
	const wantCapsuleDigest = "sha256:1119b1140fc1dd7f527c7d2f4c18125d6d6b825fd4cdcc3b06f88dcda29fb906"
	if capsuleDigest != wantCapsuleDigest {
		t.Fatalf("capsule digest = %q, want %q", capsuleDigest, wantCapsuleDigest)
	}
	capsuleKey, err := ManagerCapsuleKey(capsuleDigest)
	if err != nil {
		t.Fatal(err)
	}
	if capsuleKey != "v0/capsules/sha256/"+strings.TrimPrefix(
		wantCapsuleDigest,
		"sha256:",
	) {
		t.Fatalf("capsule key = %q", capsuleKey)
	}
	treeKey, err := ManagerTreeKey(capsule.Tree.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if treeKey != "v0/trees/sha256/"+strings.Repeat("2", 64) {
		t.Fatalf("tree key = %q", treeKey)
	}
}

func TestManagerCapsuleClosesOfficialDistribution(t *testing.T) {
	tests := []struct {
		manager      PackageManagerName
		architecture RuntimeArchitecture
		kind         ManagerEntrypointKind
		entrypoint   string
		origin       string
	}{
		{
			manager:      PackageManagerBun,
			architecture: ArchitectureAArch64,
			kind:         ManagerEntrypointNative,
			entrypoint:   managerBunEntrypoint,
			origin:       "https://github.com/oven-sh/bun/releases/download/bun-v1.3.10/bun-linux-aarch64.zip",
		},
		{
			manager:      PackageManagerBun,
			architecture: ArchitectureX8664,
			kind:         ManagerEntrypointNative,
			entrypoint:   managerBunEntrypoint,
			origin:       "https://github.com/oven-sh/bun/releases/download/bun-v1.3.10/bun-linux-x64-baseline.zip",
		},
		{
			manager:      PackageManagerNPM,
			architecture: ArchitectureAArch64,
			kind:         ManagerEntrypointNode,
			entrypoint:   managerNPMEntrypoint,
			origin:       "https://registry.npmjs.org/npm/-/npm-1.3.10.tgz",
		},
		{
			manager:      PackageManagerNPM,
			architecture: ArchitectureX8664,
			kind:         ManagerEntrypointNode,
			entrypoint:   managerNPMEntrypoint,
			origin:       "https://registry.npmjs.org/npm/-/npm-1.3.10.tgz",
		},
	}
	for _, test := range tests {
		t.Run(string(test.manager)+"/"+string(test.architecture), func(t *testing.T) {
			capsule := managerCapsuleFixture(test.manager, test.architecture)
			if capsule.Entrypoint.Kind != test.kind ||
				capsule.Entrypoint.Path != test.entrypoint ||
				capsule.Source.Origin != test.origin {
				t.Fatalf("capsule = %#v", capsule)
			}
			if _, err := CanonicalManagerCapsule(capsule); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManagerCapsuleEnforcesSourceAndTreeBounds(t *testing.T) {
	tests := []struct {
		name      string
		source    int64
		tree      int64
		wantError bool
	}{
		{name: "minimum", source: 1, tree: 1},
		{
			name:   "maximum",
			source: maxManagerDistributionBytes,
			tree:   maxManagerCapsuleTreeBytes,
		},
		{name: "zero source", source: 0, tree: 1, wantError: true},
		{name: "negative source", source: -1, tree: 1, wantError: true},
		{
			name:      "source above maximum",
			source:    maxManagerDistributionBytes + 1,
			tree:      1,
			wantError: true,
		},
		{name: "zero tree", source: 1, tree: 0, wantError: true},
		{name: "negative tree", source: 1, tree: -1, wantError: true},
		{
			name:      "tree above maximum",
			source:    1,
			tree:      maxManagerCapsuleTreeBytes + 1,
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			capsule := managerCapsuleFixture(
				PackageManagerBun,
				ArchitectureAArch64,
			)
			capsule.Source.SizeBytes = test.source
			capsule.Tree.SizeBytes = test.tree
			_, err := CanonicalManagerCapsule(capsule)
			if test.wantError && err == nil {
				t.Fatal("CanonicalManagerCapsule accepted an invalid bound")
			}
			if !test.wantError && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManagerAuthorityRejectsDivergence(t *testing.T) {
	capsule := managerCapsuleFixture(PackageManagerBun, ArchitectureAArch64)
	digest, err := ManagerCapsuleDigest(capsule)
	if err != nil {
		t.Fatal(err)
	}
	selector := NewManagerSelector(capsule.PackageManager, capsule.Architecture)
	claim := ManagerClaim{
		Architecture:         capsule.Architecture,
		FormatVersion:        ManagerSelectorFormatVersion,
		ManagerCapsuleDigest: digest,
		PackageManager:       capsule.PackageManager,
	}

	tests := map[string]func(*ManagerSelector, *ManagerClaim, *ManagerCapsule){
		"selector manager": func(selector *ManagerSelector, _ *ManagerClaim, _ *ManagerCapsule) {
			selector.PackageManager.Name = PackageManagerNPM
		},
		"claim architecture": func(_ *ManagerSelector, claim *ManagerClaim, _ *ManagerCapsule) {
			claim.Architecture = ArchitectureX8664
		},
		"capsule digest": func(_ *ManagerSelector, claim *ManagerClaim, _ *ManagerCapsule) {
			claim.ManagerCapsuleDigest = "sha256:" + strings.Repeat("9", 64)
		},
		"source origin": func(_ *ManagerSelector, _ *ManagerClaim, capsule *ManagerCapsule) {
			capsule.Source.Origin = "https://example.com/bun.zip"
		},
		"entrypoint": func(_ *ManagerSelector, _ *ManagerClaim, capsule *ManagerCapsule) {
			capsule.Entrypoint.Path = "/opt/helmr/manager/bin/other"
		},
		"tree media type": func(_ *ManagerSelector, _ *ManagerClaim, capsule *ManagerCapsule) {
			capsule.Tree.MediaType = "application/octet-stream"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changedSelector := selector
			changedClaim := claim
			changedCapsule := capsule
			mutate(&changedSelector, &changedClaim, &changedCapsule)
			if err := ValidateManagerAuthority(
				changedSelector,
				changedClaim,
				changedCapsule,
			); err == nil {
				t.Fatal("ValidateManagerAuthority accepted divergent authority")
			}
		})
	}
}

func TestManagerDocumentsRejectNonCanonicalAndOpenShapes(t *testing.T) {
	capsule := managerCapsuleFixture(PackageManagerBun, ArchitectureAArch64)
	raw, err := CanonicalManagerCapsule(capsule)
	if err != nil {
		t.Fatal(err)
	}
	nonCanonical := append([]byte(" "), raw...)
	if _, err := ParseManagerCapsule(nonCanonical); err == nil {
		t.Fatal("ParseManagerCapsule accepted non-canonical JSON")
	}
	open := bytes.Replace(
		raw,
		[]byte(`"formatVersion":0`),
		[]byte(`"formatVersion":0,"unknown":true`),
		1,
	)
	if _, err := ParseManagerCapsule(open); err == nil {
		t.Fatal("ParseManagerCapsule accepted an unknown member")
	}

	selector := NewManagerSelector(capsule.PackageManager, capsule.Architecture)
	selector.Profile = "helmr.official-managers.v1"
	if _, err := CanonicalManagerSelector(selector); err == nil {
		t.Fatal("CanonicalManagerSelector accepted another profile")
	}
	if _, err := ManagerCapsuleKey("sha256:" + strings.Repeat("A", 64)); err == nil {
		t.Fatal("ManagerCapsuleKey accepted an uppercase digest")
	}
}

func managerCapsuleFixture(
	name PackageManagerName,
	architecture RuntimeArchitecture,
) ManagerCapsule {
	manager := PackageManager{Name: name, Version: "1.3.10"}
	kind, entrypoint, origin, err := managerDistribution(manager, architecture)
	if err != nil {
		panic(err)
	}
	return ManagerCapsule{
		Architecture: architecture,
		Entrypoint: ManagerEntrypoint{
			Kind: kind,
			Path: entrypoint,
		},
		FormatVersion:  ManagerCapsuleFormatVersion,
		PackageManager: manager,
		Source: ManagerSource{
			Digest:    "sha256:" + strings.Repeat("1", 64),
			Origin:    origin,
			SizeBytes: 1,
		},
		Tree: ArtifactDescriptor{
			Digest:    "sha256:" + strings.Repeat("2", 64),
			MediaType: ManagerTreeMediaType,
			SizeBytes: 1,
		},
	}
}
