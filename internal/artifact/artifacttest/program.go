package artifacttest

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
)

// ModulePath is the program module holding the handler for digit "a" (the
// Task) or any other digit (the Actor).
func ModulePath(digit string) string {
	if digit == "a" {
		return "helmr/app/entry-0.mjs"
	}
	return "helmr/app/entry-1.mjs"
}

// AnalysisDeclarationLocator locates the handlers BuildPlan declares.
func AnalysisDeclarationLocator() artifact.DeclarationLocator {
	return artifact.DeclarationLocator{
		FormatVersion: artifact.DeclarationLocatorFormatVersion,
		Declarations: []artifact.LocatedDeclaration{
			{
				Kind:       artifact.DeclarationKindTask,
				DeclaredID: "build",
				ModulePath: ModulePath("a"),
				ExportName: "build",
				Slot:       artifact.DeclarationSlotHandler,
			},
			{
				Kind:       artifact.DeclarationKindActor,
				DeclaredID: "chat",
				ModulePath: ModulePath("b"),
				ExportName: "chat",
				Slot:       artifact.DeclarationSlotHandler,
			},
		},
	}
}

// ProgramIndex indexes BuildPlan with AnalysisDeclarationLocator and a Sandbox
// image for "repo".
func ProgramIndex(t *testing.T) artifact.ProgramIndex {
	t.Helper()
	plan := BuildPlan()
	index, err := artifact.BuildProgramIndex(
		plan,
		AnalysisDeclarationLocator(),
		map[string]definition.ComputerImage{
			"repo": {
				Profile:      definition.ComputerSeedProfile,
				Digest:       "sha256:" + strings.Repeat("d", 64),
				SizeBytes:    4096,
				MediaType:    definition.ComputerSeedMediaType,
				Architecture: definition.ArchitectureX8664,
			},
		},
		"sha256:"+strings.Repeat("4", 64),
		"sha256:"+strings.Repeat("f", 64),
	)
	if err != nil {
		t.Fatal(err)
	}
	return index
}
