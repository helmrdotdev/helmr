package deployment

import (
	"testing"
)

func TestScheduleAuthorityValidatesAcceptedScheduledTask(t *testing.T) {
	authority := &ScheduleAuthority{}
	manifest := []byte(`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":900000,"queue":"default","retry":{"enabled":false}}}`)
	_, digest, err := CanonicalManifestAndDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	queueConfig, err := CanonicalQueueConfig(QueueConfig{
		FormatVersion: BuildPlanFormatVersion,
		Queues:        []QueueInput{{Name: "default"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.ValidateScheduledTask(
		BuildPlanFormatVersion,
		"daily-report",
		manifest,
		digest[:],
		queueConfig,
	); err != nil {
		t.Fatal(err)
	}
}

func TestScheduleAuthorityRejectsPayloadlessTask(t *testing.T) {
	authority := &ScheduleAuthority{}
	manifest := []byte(`{"payload":{"kind":"none"},"run":{"maxDurationMs":900000,"queue":"default","retry":{"enabled":false}}}`)
	_, digest, err := CanonicalManifestAndDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	queueConfig, err := CanonicalQueueConfig(QueueConfig{
		FormatVersion: BuildPlanFormatVersion,
		Queues:        []QueueInput{{Name: "default"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.ValidateScheduledTask(
		BuildPlanFormatVersion,
		"daily-report",
		manifest,
		digest[:],
		queueConfig,
	); err == nil {
		t.Fatal("expected payloadless Task rejection")
	}
}
