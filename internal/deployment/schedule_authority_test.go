package deployment

import (
	"errors"
	"testing"
)

func TestScheduleAuthorityUsesManagedRuntimeAuthority(t *testing.T) {
	want := errors.New("runtime rejected")
	authority, err := NewScheduleAuthority(scheduleRuntimeResolver{
		resolve: func(string) (RuntimeDescriptor, error) {
			return RuntimeDescriptor{}, want
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := authority.ResolveRuntime("sha256:runtime"); !errors.Is(got, want) {
		t.Fatalf("ResolveRuntime error = %v", got)
	}
}

func TestScheduleAuthorityRequiresManagedRuntimeAuthority(t *testing.T) {
	if _, err := NewScheduleAuthority(nil); err == nil {
		t.Fatal("expected missing runtime authority error")
	}
}

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

type scheduleRuntimeResolver struct {
	resolve func(string) (RuntimeDescriptor, error)
}

func (r scheduleRuntimeResolver) Resolve(digest string) (RuntimeDescriptor, error) {
	return r.resolve(digest)
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
