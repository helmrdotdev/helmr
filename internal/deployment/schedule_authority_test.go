package deployment

import "testing"

func TestScheduleAuthorityValidatesAcceptedScheduledTask(t *testing.T) {
	authority := &ScheduleAuthority{}
	manifest := []byte(`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":900000,"queue":"default","retry":{"enabled":false}},"schedule":{"cron":"0 9 * * *","timezone":"UTC","workspace":{"sandboxId":"scheduler","secrets":[]}}}`)
	_, digest, err := CanonicalManifestAndDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	queueConfig, err := CanonicalQueueConfig(QueueConfig{
		FormatVersion: DeploymentPlanFormatVersion,
		Queues:        []QueueInput{{Name: "default"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.ResolveScheduledTask(
		DeploymentPlanFormatVersion,
		"daily-report",
		manifest,
		digest[:],
		queueConfig,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.ResolveScheduledTask(
		DeploymentPlanFormatVersion+1,
		"daily-report",
		manifest,
		digest[:],
		queueConfig,
	); err == nil {
		t.Fatal("wrong task manifest version was accepted")
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
		FormatVersion: DeploymentPlanFormatVersion,
		Queues:        []QueueInput{{Name: "default"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.ResolveScheduledTask(
		DeploymentPlanFormatVersion,
		"daily-report",
		manifest,
		digest[:],
		queueConfig,
	); err == nil {
		t.Fatal("expected payloadless Task rejection")
	}
}
