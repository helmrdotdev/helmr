package dbtest

import (
	"encoding/json"

	"github.com/google/uuid"
)

type ProgramReceiptAuthority struct {
	Architecture            string
	ProgramArtifactID       uuid.UUID
	ProgramDigest           string
	ProgramSizeBytes        int64
	RuntimeDigest           string
	SourceArtifactID        uuid.UUID
	SourceDigest            string
	SourceSizeBytes         int64
	StandardToolchainDigest string
}

func ProgramReceipt(authority ProgramReceiptAuthority) []byte {
	receipt := struct {
		Architecture         string `json:"architecture"`
		BuildContractVersion string `json:"buildContractVersion"`
		FormatVersion        int    `json:"formatVersion"`
		Lockfile             struct {
			Digest string `json:"digest"`
			Path   string `json:"path"`
		} `json:"lockfile"`
		Manager struct {
			Digest  string `json:"digest"`
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"manager"`
		Program struct {
			ArtifactID  string `json:"artifactId"`
			Digest      string `json:"digest"`
			IndexDigest string `json:"indexDigest"`
			MediaType   string `json:"mediaType"`
			SizeBytes   int64  `json:"sizeBytes"`
		} `json:"program"`
		Runtime struct {
			APIVersion string `json:"apiVersion"`
			Digest     string `json:"digest"`
		} `json:"runtime"`
		Source struct {
			ArtifactID string `json:"artifactId"`
			Digest     string `json:"digest"`
			MediaType  string `json:"mediaType"`
			SizeBytes  int64  `json:"sizeBytes"`
		} `json:"source"`
		StandardToolchainDigest string `json:"standardToolchainDigest"`
	}{
		Architecture:            authority.Architecture,
		BuildContractVersion:    "helmr.program-build.v0",
		FormatVersion:           0,
		StandardToolchainDigest: authority.StandardToolchainDigest,
	}
	receipt.Lockfile.Digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	receipt.Lockfile.Path = "bun.lock"
	receipt.Manager.Digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	receipt.Manager.Name = "bun"
	receipt.Manager.Version = "1.2.3"
	receipt.Program.ArtifactID = authority.ProgramArtifactID.String()
	receipt.Program.Digest = authority.ProgramDigest
	receipt.Program.IndexDigest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	receipt.Program.MediaType = "application/vnd.helmr.deployment-program.v0+squashfs"
	receipt.Program.SizeBytes = authority.ProgramSizeBytes
	receipt.Runtime.APIVersion = "helmr.runtime.v0"
	receipt.Runtime.Digest = authority.RuntimeDigest
	receipt.Source.ArtifactID = authority.SourceArtifactID.String()
	receipt.Source.Digest = authority.SourceDigest
	receipt.Source.MediaType = "application/vnd.helmr.deployment-source.v0+tar"
	receipt.Source.SizeBytes = authority.SourceSizeBytes
	raw, err := json.Marshal(receipt)
	if err != nil {
		panic(err)
	}
	return raw
}
