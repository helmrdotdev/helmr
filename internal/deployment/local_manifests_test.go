package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestLocalManifestsMatchesSharedGoldenFixture(t *testing.T) {
	fixture := loadContractFixture(t)
	var manifests LocalManifests
	if err := json.Unmarshal([]byte(fixture.LocalManifests.Canonical), &manifests); err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalLocalManifests(manifests)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != fixture.LocalManifests.Canonical {
		t.Fatalf("canonical local manifests = %q, want %q", canonical, fixture.LocalManifests.Canonical)
	}
	digest, err := LocalManifestsDigest(manifests)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(digest[:]) != fixture.LocalManifests.DigestHex {
		t.Fatalf("local manifests digest = %x, want %s", digest, fixture.LocalManifests.DigestHex)
	}
}

func TestLocalManifestsRejectsSharedMutations(t *testing.T) {
	fixture := loadContractFixture(t)
	for _, test := range fixture.LocalManifestsRejections {
		t.Run(test.Name, func(t *testing.T) {
			value := localManifestsFixtureValue(t, fixture.LocalManifests.Canonical)
			entries := value["entries"].([]any)
			switch test.Mutation {
			case "invalid_format_version":
				value["formatVersion"] = 1
			case "root_not_first":
				entries[0], entries[1] = entries[1], entries[0]
			case "duplicate_root":
				value["entries"] = append(entries, cloneLocalManifestEntry(entries[0]))
			case "entry_order":
				value["entries"] = append(entries,
					localManifestEntry("packages/z", 2),
					localManifestEntry("packages/a", 3),
				)
			case "overlapping_path":
				entries[1].(map[string]any)["path"] = "packages"
				value["entries"] = append(entries, localManifestEntry("packages/shared", 2))
			case "non_adjacent_overlapping_path":
				value["entries"] = []any{
					entries[0],
					localManifestEntry("a", 1),
					localManifestEntry("a-", 2),
					localManifestEntry("a/b", 3),
				}
			case "reserved_path":
				entries[1].(map[string]any)["path"] = "node_modules/shared"
			case "absolute_path":
				entries[1].(map[string]any)["path"] = "/packages/shared"
			case "invalid_manifest_digest":
				entries[1].(map[string]any)["manifestDigest"] = "sha256:invalid"
			default:
				t.Fatalf("unknown mutation %q", test.Mutation)
			}
			requireLocalManifestsRejection(t, value)
		})
	}
}

func TestLocalManifestsAcceptsRootOnlyAndUnsignedUTF8Order(t *testing.T) {
	rootOnly := LocalManifests{
		FormatVersion: LocalManifestsFormatVersion,
		Entries: []LocalManifestEntry{{
			ManifestDigest: digestString(0),
			Path:           ".",
		}},
	}
	if _, err := LocalManifestsDigest(rootOnly); err != nil {
		t.Fatal(err)
	}

	ordered := LocalManifests{
		FormatVersion: LocalManifestsFormatVersion,
		Entries: []LocalManifestEntry{
			rootOnly.Entries[0],
			{ManifestDigest: digestString(1), Path: "packages/z"},
			{ManifestDigest: digestString(2), Path: "packages/é"},
		},
	}
	if _, err := LocalManifestsDigest(ordered); err != nil {
		t.Fatal(err)
	}
}

func TestLocalManifestsRejectsInvalidCollectionsAndPathBounds(t *testing.T) {
	fixture := loadContractFixture(t)
	var manifests LocalManifests
	if err := json.Unmarshal([]byte(fixture.LocalManifests.Canonical), &manifests); err != nil {
		t.Fatal(err)
	}
	manifests.Entries[1].Path = strings.Repeat("a", maxPackagePathComponent+1)
	if _, err := LocalManifestsDigest(manifests); err == nil {
		t.Fatal("LocalManifestsDigest accepted an oversized path component")
	}
	manifests.Entries = nil
	if _, err := LocalManifestsDigest(manifests); err == nil {
		t.Fatal("LocalManifestsDigest accepted nil entries")
	}
}

func localManifestsFixtureValue(t *testing.T, canonical string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(canonical), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func requireLocalManifestsRejection(t *testing.T, value map[string]any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var manifests LocalManifests
	if err := json.Unmarshal(raw, &manifests); err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalLocalManifests(manifests); err == nil {
		t.Fatal("CanonicalLocalManifests returned nil error")
	}
}

func localManifestEntry(path string, fill byte) map[string]any {
	return map[string]any{
		"manifestDigest": digestString(fill),
		"path":           path,
	}
}

func cloneLocalManifestEntry(value any) map[string]any {
	entry := value.(map[string]any)
	return map[string]any{
		"manifestDigest": entry["manifestDigest"],
		"path":           entry["path"],
	}
}

func digestString(fill byte) string {
	return "sha256:" + strings.Repeat(string("0123456789abcdef"[fill%16]), sha256.Size*2)
}
