package artifact

import (
	"crypto/sha256"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func testDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return sha256sum.FormatDigest(digest[:])
}

func testAnalysisDeclarationLocator() DeclarationLocator {
	return DeclarationLocator{
		FormatVersion: DeclarationLocatorFormatVersion,
		Declarations: []LocatedDeclaration{
			{
				Kind:       DeclarationKindTask,
				DeclaredID: "build",
				ModulePath: testModulePath("a"),
				ExportName: "build",
				Slot:       DeclarationSlotHandler,
			},
			{
				Kind:       DeclarationKindActor,
				DeclaredID: "chat",
				ModulePath: testModulePath("b"),
				ExportName: "chat",
				Slot:       DeclarationSlotHandler,
			},
		},
	}
}
