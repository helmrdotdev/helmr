package bundle

import (
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"testing"
)

func TestPreparationRequiresExactIndependentProjection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Manifest)
	}{
		{"missing", func(m *Manifest) { m.Preparations = nil }},
		{"Session Program", func(m *Manifest) { m.Preparations[0] = m.Program }},
		{"Template evaluation", func(m *Manifest) { m.Preparations[0].Metadata.Definitions[0].Template.Resources.MemoryMiB++ }},
		{"Runtime", func(m *Manifest) { m.Preparations[0].Metadata.RuntimeDigest = artifacttest.Digest("other Runtime") }},
		{"same digest conflicting metadata", func(m *Manifest) { m.Preparations[0].Artifact = m.Program.Artifact }},
		{"duplicate", func(m *Manifest) { m.Preparations = append(m.Preparations, m.Preparations[0]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testDeploymentBundle(t)
			tc.change(&m)
			if _, err := Canonical(m); err == nil {
				t.Fatal("invalid independent preparation admitted")
			}
		})
	}
}

func TestPreparationIdentityDoesNotDependOnAgentOnlyProgram(t *testing.T) {
	m := testDeploymentBundle(t)
	original := m.Preparations[0]
	m.Program.Artifact.Digest = artifacttest.Digest("changed Agent body")
	if err := validatePreparations(m); err != nil {
		t.Fatal(err)
	}
	got, ok := m.Preparation("repo")
	if !ok || got.Artifact != original.Artifact {
		t.Fatal("preparation followed Session Program")
	}
	if _, ok := m.ProgramObject(original.Artifact.Digest); !ok {
		t.Fatal("preparation artifact omitted from verification")
	}
	if _, ok := m.Preparation("absent"); ok {
		t.Fatal("unknown Template selected")
	}
}

func TestPreparationAllowsTemplateOnlySessionArtifact(t *testing.T) {
	m := testDeploymentBundle(t)
	m.Program = m.Preparations[0]
	m.Preparations = []artifact.ProgramOutput{m.Program}
	if err := validatePreparations(m); err != nil {
		t.Fatal(err)
	}
}

func TestSeedOnlyTemplateRequiresNoPreparationArtifact(t *testing.T) {
	m := testDeploymentBundle(t)
	for i := range m.Program.Metadata.Definitions {
		if m.Program.Metadata.Definitions[i].Template != nil {
			m.Program.Metadata.Definitions[i].Template.Prepare = false
		}
	}
	m.Preparations = []artifact.ProgramOutput{}
	if err := validatePreparations(m); err != nil {
		t.Fatal(err)
	}
	m.Preparations = []artifact.ProgramOutput{m.Program}
	if err := validatePreparations(m); err == nil {
		t.Fatal("seed-only Template has redundant artifact")
	}
}
