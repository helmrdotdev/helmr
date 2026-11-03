package release

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func testRelease(t *testing.T) ([]byte, []byte) {
	t.Helper()
	index, err := os.ReadFile("testdata/v0.2.9-release-index.json")
	if err != nil {
		t.Fatal(err)
	}
	signature, err := os.ReadFile("testdata/v0.2.9-release-index.sigstore.json")
	if err != nil {
		t.Fatal(err)
	}
	return index, signature
}

// A published release verifies offline against the embedded trusted root.
func TestVerifyAcceptsAPublishedRelease(t *testing.T) {
	index, signature := testRelease(t)
	verified, err := Verify(index, signature, "v0.2.9")
	if err != nil {
		t.Fatal(err)
	}
	if verified.Version != "v0.2.9" || verified.Assets[HostArtifacts].Digest == "" {
		t.Fatalf("verified = %+v", verified)
	}
}

// Altered bytes, another version's signer or a malformed signature are refused.
func TestVerifyRefusesAnythingElse(t *testing.T) {
	index, signature := testRelease(t)
	altered := bytes.Replace(index, []byte(`"v0.2.9"`), []byte(`"v0.2.8"`), 1)
	for name, check := range map[string]func() error{
		"altered index": func() error { _, err := Verify(altered, signature, "v0.2.8"); return err },
		"other version": func() error { _, err := Verify(index, signature, "v0.2.8"); return err },
		"preview signer": func() error {
			_, err := Verify(index, signature, "v0.2.9-preview.gabcdef01.b2")
			return err
		},
		"malformed signature": func() error { _, err := Verify(index, []byte("{}"), "v0.2.9"); return err },
		"invalid version":     func() error { _, err := Verify(index, signature, "latest"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if check() == nil {
				t.Fatal("verified")
			}
		})
	}
}

func TestSignerFollowsTheReleaseWorkflow(t *testing.T) {
	tag, err := Signer("v1.2.3")
	if err != nil || !strings.HasSuffix(tag, "@refs/tags/v1.2.3") {
		t.Fatalf("tag signer = %q, %v", tag, err)
	}
	preview, err := Signer("v1.2.3-preview.gabcdef01.b9")
	if err != nil || !strings.HasSuffix(preview, "@refs/heads/main") {
		t.Fatalf("preview signer = %q, %v", preview, err)
	}
}

func TestParseRequiresEveryHostAsset(t *testing.T) {
	index, _ := testRelease(t)
	if _, err := parse(index, "v0.2.9"); err != nil {
		t.Fatal(err)
	}
	missing := bytes.Replace(index, []byte(`"runtime-artifacts.tar"`), []byte(`"runtime-artifacts.gone"`), 1)
	if _, err := parse(missing, "v0.2.9"); err == nil {
		t.Fatal("index without the runtime artifacts parsed")
	}
}
