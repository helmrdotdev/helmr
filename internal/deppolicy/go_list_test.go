package deppolicy

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func goList(t *testing.T, root, goos string, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list", "-buildvcs=false"}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v for %s: %v\n%s", args, goos, err, stderr.String())
	}
	return string(output)
}
