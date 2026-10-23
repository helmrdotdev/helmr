package clickhouse

import (
	"os"
	"os/exec"
	"testing"
	"uuid"
)

func TestCanaryScriptRoundTrip(t *testing.T) {
	if os.Getenv("HELMR_TEST_CLICKHOUSE_BOOTSTRAP") != "1" {
		t.Skip("set HELMR_TEST_CLICKHOUSE_BOOTSTRAP=1 to start an owned disposable local server")
	}
	cfg := startClickHouseTestServer(t)
	environmentID, commandID := uuid.NewV7().String(), uuid.New().String()
	command := exec.CommandContext(t.Context(), "bash", "scripts/clickhouse-canary.sh")
	command.Dir = "../.."
	command.Env = append(os.Environ(),
		"CLICKHOUSE_URL="+cfg.URL, "CLICKHOUSE_USER="+cfg.User, "CLICKHOUSE_PASSWORD="+cfg.Password,
		"HELMR_CLICKHOUSE_MIGRATIONS=internal/clickhouse/schema/migrations",
		"HELMR_CLICKHOUSE_CANARY_ENVIRONMENT_ID="+environmentID,
		"HELMR_CLICKHOUSE_CANARY_COMMAND_ID="+commandID,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("canary: %v\n%s", err, output)
	}
	if want := "clickhouse canary ok environment_id=" + environmentID + " command_id=" + commandID + "\n"; string(output) != want {
		t.Fatalf("canary output = %q, want %q", output, want)
	}
}
