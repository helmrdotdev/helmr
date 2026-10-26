package artifact

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/version"
)

func TestNodeVersionAuthority(t *testing.T) {
	if _, err := NodeLanguageFlags(version.Node()); err != nil {
		t.Fatal(err)
	}
	for _, previous := range []string{"24.19.0", "24.21.0"} {
		if _, err := NodeLanguageFlags(previous); err == nil {
			t.Fatalf("accepted unsupported Node language version %s", previous)
		}
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
