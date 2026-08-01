package workergroup

import "testing"

func TestConfigPrepareResolvesEnrollmentSecretWithoutInfrastructureAuthority(t *testing.T) {
	groups, err := DecodeConfig(`[{"id":"run-workers","name":"Run workers","enrollment_secret_env":"HELMR_WORKER_ENROLLMENT_SECRET_RUN","allows_run":true,"allows_build":false,"observation_ttl_seconds":120,"instance_capacity":{"milli_cpu":1000,"memory_bytes":1024,"guest_ephemeral_disk_bytes":2048,"vm_slots":1}}]`)
	if err != nil {
		t.Fatal(err)
	}
	desired, secret, err := groups[0].Prepare(func(name string) (string, bool) {
		if name != "HELMR_WORKER_ENROLLMENT_SECRET_RUN" {
			t.Fatalf("secret env = %q", name)
		}
		return "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8", true
	})
	if err != nil {
		t.Fatal(err)
	}
	if desired.Spec.ID != "run-workers" || !desired.Spec.AllowsRun || desired.Spec.AllowsBuild || secret.GroupID != "run-workers" {
		t.Fatalf("desired = %#v secret = %#v", desired, secret)
	}
}

func TestConfigRejectsInfrastructureFields(t *testing.T) {
	if _, err := DecodeConfig(`[{"id":"run-workers","region":"us-east-1"}]`); err == nil {
		t.Fatal("infrastructure field was accepted")
	}
}

func TestConfigPrepareRequiresExplicitSecretEnvironment(t *testing.T) {
	group := Config{
		ID: "run-workers", Name: "Run workers", EnrollmentSecretEnv: "HELMR_WORKER_ENROLLMENT_SECRET_RUN",
		AllowsRun: true, ObservationTTL: 120,
		InstanceCapacity: Capacity{MilliCPU: 1000, MemoryBytes: 1024, GuestEphemeralDiskBytes: 2048, VMSlots: 1},
	}
	if _, _, err := group.Prepare(func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("missing enrollment secret was accepted")
	}
}
