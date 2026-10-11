package artifact

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/definition"
	"testing"
)

func TestProgramMetadataCloneOwnsNestedValues(t *testing.T) {
	index := testProgramMetadata(t)
	a := index.Definitions[0].Agent
	a.MaxTurnDurationMs = new(int64(100))
	a.CloseAfterIdleMs = new(int64(200))
	c := index.Definitions[2].Computer
	c.Resources.DiskMiB = new(int64(500))
	c.Refresh = &definition.ComputerRefresh{EveryMs: 1000, MaxAgeMs: new(int64(2000))}
	c.Seed.Config.Env = []string{"KEY=value"}
	c.Seed.Config.Cmd = []string{"true"}
	c.Seed.Config.Entrypoint = []string{"sh"}
	c.Secrets = []definition.SecretBinding{{SecretID: "original", Env: &definition.SecretBindingEnv{Name: "KEY", Mode: "raw"}}}
	c.BuildSecrets = []definition.SecretBinding{{SecretID: "original", Env: &definition.SecretBindingEnv{Name: "BUILD", Mode: "raw"}}}
	before, _ := json.Marshal(index)
	clone := index.Clone()
	*clone.Definitions[0].Agent.MaxTurnDurationMs = 101
	*clone.Definitions[0].Agent.CloseAfterIdleMs = 201
	clone.Definitions[0].Agent.Triggers["daily"].Input[0] = 'x'
	clone.Definitions[0].Agent.Triggers["new"] = definition.CronTrigger{}
	cc := clone.Definitions[2].Computer
	*cc.Resources.DiskMiB = 501
	cc.Refresh.EveryMs = 1001
	*cc.Refresh.MaxAgeMs = 2001
	cc.Seed.Config.Env[0] = "changed"
	cc.Seed.Config.Cmd[0] = "changed"
	cc.Seed.Config.Entrypoint[0] = "changed"
	cc.Secrets[0].SecretID = "changed"
	cc.BuildSecrets[0].SecretID = "changed"
	after, _ := json.Marshal(index)
	if string(before) != string(after) {
		t.Fatal("clone mutation changed original")
	}
}
