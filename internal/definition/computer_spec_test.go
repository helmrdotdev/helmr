package definition

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
)

func computerSpecFixture() (ComputerConfig, cas.Descriptor) {
	config := ComputerConfig{Architecture: ArchitectureX8664, RuntimeContract: RuntimeContract, Profile: ComputerSeedProfile, Resources: ResourcesManifest{MilliCPU: 1000, MemoryMiB: 512}}
	seed := cas.Descriptor{Digest: "sha256:" + strings.Repeat("a", 64), MediaType: ComputerSeedMediaType, SizeBytes: 4096}
	return config, seed
}

func parseComputerSpecFixture(config ComputerConfig, seed cas.Descriptor) (ComputerSpec, error) {
	raw, err := json.Marshal(config)
	if err != nil {
		return ComputerSpec{}, err
	}
	return ParseComputerSpec(raw, seed)
}

func TestComputerSpecCanonicalIdentity(t *testing.T) {
	manifest, image := computerSpecFixture()
	spec, err := parseComputerSpecFixture(manifest, image)
	if err != nil {
		t.Fatal(err)
	}
	const expectedConfig = `{"architecture":"x86_64","image":{"Cmd":[],"Entrypoint":[],"Env":[],"User":"","WorkingDir":""},"profile":"linux-amd64-ext4-v1","resources":{"memoryMiB":512,"milliCpu":1000},"runtimeContract":"helmr.runtime.v0"}`
	if string(spec.Config) != expectedConfig {
		t.Fatalf("config = %s", spec.Config)
	}
	const expectedDigest = "f80a81a1ac4b5835510bd16578565ecc15b2b60cf3606c48300f13e8fa7ccf67"
	if actual := hex.EncodeToString(spec.Digest[:]); actual != expectedDigest {
		t.Fatalf("digest = %s", actual)
	}

	var reordered map[string]any
	if err := json.Unmarshal(spec.Config, &reordered); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(reordered, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := ParseComputerSpec(raw, spec.Seed)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.Digest != spec.Digest || !bytes.Equal(roundTrip.Config, spec.Config) {
		t.Fatal("JSON round trip changed identity")
	}
}

func TestComputerSpecLaunchChangesChangeIdentity(t *testing.T) {
	manifest, image := computerSpecFixture()
	original, err := parseComputerSpecFixture(manifest, image)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		edit func(*ComputerConfig, *cas.Descriptor)
	}{
		{"seed bytes", func(m *ComputerConfig, i *cas.Descriptor) {
			i.Digest = "sha256:" + strings.Repeat("b", 64)
		}},
		{"seed size", func(m *ComputerConfig, i *cas.Descriptor) { i.SizeBytes++ }},
		{"cpu", func(m *ComputerConfig, i *cas.Descriptor) { m.Resources.MilliCPU++ }},
		{"memory", func(m *ComputerConfig, i *cas.Descriptor) { m.Resources.MemoryMiB++ }},
		{"image env", func(m *ComputerConfig, i *cas.Descriptor) {
			m.Image.Env = []string{"A=one", "A=two"}
		}},
		{"image user", func(m *ComputerConfig, i *cas.Descriptor) {
			m.Image.User = "1000"
		}},
		{"working directory", func(m *ComputerConfig, i *cas.Descriptor) {
			m.Image.WorkingDir = "/app"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, i := computerSpecFixture()
			test.edit(&m, &i)
			changed, err := parseComputerSpecFixture(m, i)
			if err != nil {
				t.Fatal(err)
			}
			if changed.Digest == original.Digest {
				t.Fatal("launch change retained identity")
			}
		})
	}
}

func TestComputerSpecRejectsAmbiguousAndUnknownConfig(t *testing.T) {
	manifest, image := computerSpecFixture()
	spec, err := parseComputerSpecFixture(manifest, image)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		strings.Replace(string(spec.Config), `"architecture":"x86_64"`, `"architecture":"x86_64","architecture":"x86_64"`, 1),
		strings.Replace(string(spec.Config), `"resources":`, `"unknown":true,"resources":`, 1),
		strings.Replace(string(spec.Config), `"milliCpu":1000`, `"milliCpu":0`, 1),
		strings.Replace(string(spec.Config), `"x86_64"`, `"arm64"`, 1),
		`null`,
		string(spec.Config) + `{}`,
	} {
		if _, err := ParseComputerSpec([]byte(raw), spec.Seed); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	image.MediaType = "application/octet-stream"
	if _, err := parseComputerSpecFixture(manifest, image); err == nil {
		t.Fatal("accepted unsupported seed descriptor")
	}
}
