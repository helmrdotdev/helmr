package deployment

import (
	"github.com/helmrdotdev/helmr/internal/version"
	"testing"
)

func TestNodeVersionAuthority(t *testing.T) {
	if _, err := NodeLanguageFlags(version.Node()); err != nil {
		t.Fatal(err)
	}
	if _, err := NodeLanguageFlags("24.19.0"); err == nil {
		t.Fatal("accepted wrong Node language version")
	}
	result := testProgramCompilerResult(t)
	result.NodeVersion = version.Node()
	if err := validateProgramCompilerResult(result); err != nil {
		t.Fatal(err)
	}
	result.NodeVersion = "24.19.0"
	if err := validateProgramCompilerResult(result); err == nil {
		t.Fatal("accepted wrong compiler Node version")
	}
}
