package deployment

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

func TestProgramIndexCloneOwnsNestedValues(t *testing.T) {
	ttl := int64(100)
	attempts := int64(3)
	limit := int64(2)
	run := definition.RunManifest{TTLMs: &ttl, Retry: definition.RetryManifest{MaxAttempts: &attempts, Backoff: &definition.RetryBackoff{}}}
	index := ProgramIndex{Declarations: []ProgramIndexDeclaration{
		{Task: &definition.TaskManifest{Run: run, Schedule: &definition.ScheduleManifest{Computer: definition.ScheduleComputerManifest{Secrets: []secretbinding.Binding{{Name: "original"}}}}}, Locator: &ProgramLocator{ExportName: "original"}},
		{Actor: &definition.ActorManifest{Run: run}},
		{Sandbox: &definition.SandboxManifest{}},
	}, Queues: []definition.QueueInput{{Name: "original", ConcurrencyLimit: &limit}}}
	clone := index.Clone()
	*clone.Declarations[0].Task.Run.TTLMs = 200
	*clone.Declarations[0].Task.Run.Retry.MaxAttempts = 9
	clone.Declarations[0].Task.Run.Retry.Backoff.MinMs = 123
	clone.Declarations[0].Task.Schedule.Computer.Secrets[0].Name = "changed"
	clone.Declarations[0].Locator.ExportName = "changed"
	*clone.Declarations[1].Actor.Run.TTLMs = 300
	*clone.Declarations[1].Actor.Run.Retry.MaxAttempts = 10
	clone.Declarations[1].Actor.Run.Retry.Backoff.MinMs = 456
	clone.Declarations[2].Sandbox.Resources.MemoryMiB = 999
	*clone.Queues[0].ConcurrencyLimit = 99
	clone.Queues[0].Name = "changed"
	for _, original := range []definition.RunManifest{index.Declarations[0].Task.Run, index.Declarations[1].Actor.Run} {
		if *original.TTLMs != 100 || *original.Retry.MaxAttempts != 3 || original.Retry.Backoff.MinMs != 0 {
			t.Errorf("clone mutation changed source Run: ttl=%d attempts=%d backoff=%d", *original.TTLMs, *original.Retry.MaxAttempts, original.Retry.Backoff.MinMs)
		}
	}
	if index.Declarations[0].Task.Schedule.Computer.Secrets[0].Name != "original" ||
		index.Declarations[0].Locator.ExportName != "original" ||
		index.Declarations[2].Sandbox.Resources.MemoryMiB != 0 ||
		*index.Queues[0].ConcurrencyLimit != 2 || index.Queues[0].Name != "original" {
		t.Error("clone mutation changed source schedule, locator, sandbox or queue")
	}
	if got := (ProgramIndex{Declarations: []ProgramIndexDeclaration{}, Queues: []definition.QueueInput{}}).Clone(); got.Declarations == nil || got.Queues == nil {
		t.Fatal("clone lost canonical empty arrays")
	}
}
