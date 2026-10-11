package artifact

import (
	"crypto/sha256"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func testDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return sha256sum.FormatDigest(digest[:])
}

func testDefinitionIndex() DefinitionIndex {
	return DefinitionIndex{APIVersion: "helmr.definition-index.v1", Agents: []AgentBundleEntry{
		{ID: "build", ComputerDefinitionID: "repo", ModulePath: testModulePath("a"), ExportName: "build"},
		{ID: "chat", ComputerDefinitionID: "repo", ModulePath: testModulePath("b"), ExportName: "chat"},
	}, Computers: []ComputerBundleEntry{{ID: "repo", ModulePath: testModulePath("a"), ExportName: "build", ThroughAgent: true}}}
}
