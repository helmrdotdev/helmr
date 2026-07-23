package deployment

import "testing"

func TestResolveActorRunAdmissionPinsDefinitionAndQueueAuthority(t *testing.T) {
	manifest := []byte(`{"idleTimeoutMs":30000,"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false},"ttlMs":60000}}`)
	_, digest, err := CanonicalManifestAndDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	queueConfig := []byte(`{"formatVersion":0,"queues":[{"concurrencyLimit":2,"name":"default"},{"name":"priority"}]}`)
	admission, err := ResolveActorRunAdmission(
		0, "operator.v1", manifest, digest[:], queueConfig, "priority",
	)
	if err != nil {
		t.Fatal(err)
	}
	if admission.QueueName != "priority" || admission.QueueConcurrencyLimit != nil ||
		admission.MaxActiveDurationMS != 300_000 ||
		admission.QueuedTTLMS == nil || *admission.QueuedTTLMS != 60_000 ||
		string(admission.RetryPolicy) != `{"enabled":false}` {
		t.Fatalf("admission = %+v", admission)
	}
}

func TestResolveActorRunAdmissionRejectsUndefinedQueueAndDigestMismatch(t *testing.T) {
	manifest := []byte(`{"idleTimeoutMs":30000,"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`)
	_, digest, err := CanonicalManifestAndDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	queueConfig := []byte(`{"formatVersion":0,"queues":[{"name":"default"}]}`)
	if _, err := ResolveActorRunAdmission(
		0, "operator.v1", manifest, digest[:], queueConfig, "missing",
	); err == nil {
		t.Fatal("undefined queue was accepted")
	}
	digest[0] ^= 0xff
	if _, err := ResolveActorRunAdmission(
		0, "operator.v1", manifest, digest[:], queueConfig, "",
	); err == nil {
		t.Fatal("manifest digest mismatch was accepted")
	}
}

func TestResolveActorRunAdmissionOverrideDoesNotMaskInvalidStoredDefault(t *testing.T) {
	manifest := []byte(`{"idleTimeoutMs":30000,"run":{"maxDurationMs":300000,"queue":"missing","retry":{"enabled":false}}}`)
	_, digest, err := CanonicalManifestAndDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	queueConfig := []byte(`{"formatVersion":0,"queues":[{"name":"priority"}]}`)
	if _, err := ResolveActorRunAdmission(
		0, "operator.v1", manifest, digest[:], queueConfig, "priority",
	); err == nil {
		t.Fatal("queue override masked an invalid stored default")
	}
}
