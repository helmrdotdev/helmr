// Package release reads Helmr's signed release index and verifies its
// signature, so a machine installs or updates only bytes the release workflow
// published for that version.
package release

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// IndexSchema is the release index schema this package reads.
const IndexSchema = "helmr.release.v0"

const (
	signerWorkflow = "https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml"
	signerIssuer   = "https://token.actions.githubusercontent.com"
)

// The Sigstore public-good trusted root, embedded so a machine verifies a
// release with no network beyond the Control Plane and its storage.
//
//go:embed trusted_root.json
var trustedRoot []byte

var (
	versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)
	digestPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Asset is one released file.
type Asset struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
}

// Index is a release's signed list of assets.
type Index struct {
	Schema       string           `json:"schema"`
	Version      string           `json:"version"`
	SourceCommit string           `json:"sourceCommit"`
	Assets       map[string]Asset `json:"assets"`
}

// The worker host installs these assets.
const (
	HostArtifacts            = "worker-host-artifacts.tar"
	HostArtifactsManifest    = "worker-host-artifacts.json"
	RuntimeArtifacts         = "runtime-artifacts.tar"
	RuntimeArtifactsManifest = "runtime-artifacts.json"
	LinuxCLI                 = "helmr-linux-amd64.tar.gz"
)

// HostAssets are the assets a worker host needs to install or update.
var HostAssets = []string{HostArtifacts, HostArtifactsManifest, RuntimeArtifacts, RuntimeArtifactsManifest, LinuxCLI}

// ValidVersion reports whether version names a release.
func ValidVersion(version string) bool { return versionPattern.MatchString(version) }

// Signer is the workflow identity that signs version's index: the release
// workflow at the version's tag, or on main for a preview.
func Signer(version string) (string, error) {
	if !ValidVersion(version) {
		return "", fmt.Errorf("invalid release version %q", version)
	}
	ref := "refs/tags/" + version
	if strings.Contains(version, "-preview.") {
		ref = "refs/heads/main"
	}
	return signerWorkflow + "@" + ref, nil
}

// Verify checks that sigstoreBundle is the release workflow's signature of
// index for version, then parses the index and checks that it names version
// and lists every host asset.
func Verify(index, sigstoreBundle []byte, version string) (Index, error) {
	identity, err := Signer(version)
	if err != nil {
		return Index{}, err
	}
	var signature bundle.Bundle
	if err := signature.UnmarshalJSON(sigstoreBundle); err != nil {
		return Index{}, fmt.Errorf("read release index signature: %w", err)
	}
	material, err := root.NewTrustedRootFromJSON(trustedRoot)
	if err != nil {
		return Index{}, fmt.Errorf("read Sigstore trusted root: %w", err)
	}
	verifier, err := verify.NewVerifier(material, verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return Index{}, fmt.Errorf("configure release verification: %w", err)
	}
	certificate, err := verify.NewShortCertificateIdentity(signerIssuer, "", identity, "")
	if err != nil {
		return Index{}, err
	}
	if _, err := verifier.Verify(&signature, verify.NewPolicy(verify.WithArtifact(bytes.NewReader(index)), verify.WithCertificateIdentity(certificate))); err != nil {
		return Index{}, fmt.Errorf("release index for %s is not signed by the release workflow: %w", version, err)
	}
	return parse(index, version)
}

func parse(raw []byte, version string) (Index, error) {
	var index Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return Index{}, fmt.Errorf("read release index: %w", err)
	}
	if index.Schema != IndexSchema || index.Version != version {
		return Index{}, fmt.Errorf("release index is %s %s, want %s %s", index.Schema, index.Version, IndexSchema, version)
	}
	for _, name := range HostAssets {
		asset, ok := index.Assets[name]
		if !ok {
			return Index{}, fmt.Errorf("release %s does not list %s", version, name)
		}
		if !digestPattern.MatchString(asset.Digest) || asset.SizeBytes <= 0 {
			return Index{}, errors.New("release index lists an invalid asset " + name)
		}
	}
	return index, nil
}
