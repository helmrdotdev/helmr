package version

import "testing"

func TestString(t *testing.T) {
	originalVersion, originalSourceCommit := Version, SourceCommit
	Version = "v1.2.3-test"
	SourceCommit = "0123456789abcdef0123456789abcdef01234567"
	t.Cleanup(func() {
		Version, SourceCommit = originalVersion, originalSourceCommit
	})

	const want = "v1.2.3-test (0123456789abcdef0123456789abcdef01234567)"
	if got := String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
