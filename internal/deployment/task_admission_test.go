package deployment

import "testing"

func TestResolveTaskRunAdmissionAppliesValidatedOverrides(t *testing.T) {
	manifest := []byte(`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false},"ttlMs":60000}}`)
	_, digest, err := CanonicalManifestAndDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	queueConfig := []byte(`{"formatVersion":0,"queues":[{"concurrencyLimit":2,"name":"default"},{"name":"priority"}]}`)
	ttl := int64(120_000)
	admission, err := ResolveTaskRunAdmission(
		DeploymentPlanFormatVersion,
		"resize-image",
		manifest,
		digest[:],
		queueConfig,
		"priority",
		&ttl,
		[]byte(`{"backoff":{"factor":2,"jitter":"full","maxMs":30000,"minMs":1000},"enabled":true,"maxAttempts":3}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !admission.HasPayload || admission.QueueName != "priority" ||
		admission.QueueConcurrencyLimit != nil ||
		admission.MaxActiveDurationMS != 300_000 ||
		admission.QueuedTTLMS == nil || *admission.QueuedTTLMS != ttl ||
		string(admission.RetryPolicy) != `{"backoff":{"factor":2,"jitter":"full","maxMs":30000,"minMs":1000},"enabled":true,"maxAttempts":3}` {
		t.Fatalf("admission = %+v", admission)
	}
}

func TestResolveTaskRunAdmissionRejectsInvalidStoredAuthority(t *testing.T) {
	manifest := []byte(`{"payload":{"kind":"none"},"run":{"maxDurationMs":300000,"queue":"missing","retry":{"enabled":false}}}`)
	_, digest, err := CanonicalManifestAndDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	queueConfig := []byte(`{"formatVersion":0,"queues":[{"name":"priority"}]}`)
	if _, err := ResolveTaskRunAdmission(
		DeploymentPlanFormatVersion, "heartbeat", manifest, digest[:], queueConfig, "priority", nil, nil,
	); err == nil {
		t.Fatal("queue override masked an invalid stored default")
	}
}
