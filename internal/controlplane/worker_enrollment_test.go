package controlplane

import (
	"bytes"
	"testing"

	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestStrictWorkerEnrollmentBearer(t *testing.T) {
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	got, err := strictWorkerEnrollmentBearer([]string{"Bearer " + token.Raw})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Hash, token.Hash) || got.SingleUse {
		t.Fatalf("bearer = %+v, want the group token hash", got)
	}
	single, err := auth.GenerateSingleUseEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	got, err = strictWorkerEnrollmentBearer([]string{"Bearer " + single.Raw})
	if err != nil || !bytes.Equal(got.Hash, single.Hash) || !got.SingleUse {
		t.Fatalf("single-use bearer = %+v, %v", got, err)
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
