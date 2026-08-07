package controlplane

import (
	"bytes"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestStrictWorkerEnrollmentBearer(t *testing.T) {
	token, err := workergroup.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	got, err := strictWorkerEnrollmentBearer([]string{"Bearer " + token.Raw})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, token.Hash) {
		t.Fatal("bearer hash did not match token")
	}
	for _, values := range [][]string{
		nil,
		{"bearer " + token.Raw},
		{"Bearer  " + token.Raw},
		{"Bearer " + token.Raw + "\n"},
		{"Bearer " + token.Raw, "Bearer " + token.Raw},
	} {
		if _, err := strictWorkerEnrollmentBearer(values); err == nil {
			t.Fatalf("invalid Authorization values accepted: %q", values)
		}
	}
}
