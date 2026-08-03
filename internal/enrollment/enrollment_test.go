package enrollment

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const testSecret = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"

func TestVerifierAcceptsBoundEnrollmentRequest(t *testing.T) {
	verifier, err := NewVerifier([]GroupSecret{{GroupID: "run-workers", Secret: testSecret}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := BuildRequest("run-workers", "fresh-nonce", true, false, "host-1", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if request.Proof != "haOpQjqAKMs1qpvLfVzbpXjLMsfM4hAGNNkCqtHWuf8" {
		t.Fatalf("proof = %q", request.Proof)
	}
	if err := verifier.Verify(request); err != nil {
		t.Fatal(err)
	}
}

func TestVerifierRejectsEveryTamperedField(t *testing.T) {
	verifier, err := NewVerifier([]GroupSecret{{GroupID: "run-workers", Secret: testSecret}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := BuildRequest("run-workers", "fresh-nonce", true, true, "host-1", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*workerapi.EnrollmentRequest){
		"group":       func(r *workerapi.EnrollmentRequest) { r.WorkerGroupID = "other-workers" },
		"nonce":       func(r *workerapi.EnrollmentRequest) { r.Nonce = "other-nonce" },
		"run role":    func(r *workerapi.EnrollmentRequest) { r.SupportsRun = false },
		"build role":  func(r *workerapi.EnrollmentRequest) { r.SupportsBuild = false },
		"resource id": func(r *workerapi.EnrollmentRequest) { r.ResourceID = "host-2" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			tampered := request
			mutate(&tampered)
			if err := verifier.Verify(tampered); err == nil {
				t.Fatal("tampered enrollment request was accepted")
			}
		})
	}
}

func TestVerifierRejectsNonCanonicalProof(t *testing.T) {
	verifier, err := NewVerifier([]GroupSecret{{GroupID: "run-workers", Secret: testSecret}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := BuildRequest("run-workers", "fresh-nonce", true, false, "host-1", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	request.Proof += "="
	if err := verifier.Verify(request); err == nil {
		t.Fatal("padded proof was accepted")
	}
}

func TestVerifierRequiresConfiguredCanonicalGroupsAndStrongSecrets(t *testing.T) {
	for name, groups := range map[string][]GroupSecret{
		"empty":           nil,
		"noncanonical id": {{GroupID: " run-workers", Secret: testSecret}},
		"short secret":    {{GroupID: "run-workers", Secret: "short"}},
		"duplicate":       {{GroupID: "run-workers", Secret: testSecret}, {GroupID: "run-workers", Secret: testSecret}},
		"shared secret":   {{GroupID: "run-workers", Secret: testSecret}, {GroupID: "build-workers", Secret: testSecret}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewVerifier(groups); err == nil {
				t.Fatal("invalid enrollment configuration was accepted")
			}
		})
	}
}
