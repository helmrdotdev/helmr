package bundle

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
)

func testProgramIndex(t *testing.T) artifact.ProgramIndex {
	t.Helper()
	plan := testBuildPlan()
	index, err := artifact.BuildProgramIndex(
		plan,
		testAnalysisDeclarationLocator(),
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

func testAnalysisDeclarationLocator() artifact.DeclarationLocator {
	return artifact.DeclarationLocator{
		FormatVersion: artifact.DeclarationLocatorFormatVersion,
		Declarations: []artifact.LocatedDeclaration{
			{
				Kind:       artifact.DeclarationKindTask,
				DeclaredID: "build",
				ModulePath: testModulePath("a"),
				ExportName: "build",
				Slot:       artifact.DeclarationSlotHandler,
			},
			{
				Kind:       artifact.DeclarationKindActor,
				DeclaredID: "chat",
				ModulePath: testModulePath("b"),
				ExportName: "chat",
				Slot:       artifact.DeclarationSlotHandler,
			},
		},
	}
}

func testModulePath(digit string) string {
	if digit == "a" {
		return "helmr/app/entry-0.mjs"
	}
	return "helmr/app/entry-1.mjs"
}
