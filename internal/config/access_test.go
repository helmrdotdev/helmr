package config

import (
	"net/http"
	"testing"
)

func TestAccessHTTPClientConfiguration(t *testing.T) {
	for _, key := range []string{"HELMR_ACCESS_ORIGIN", "HELMR_ACCESS_CLIENT_ID", "HELMR_ACCESS_CLIENT_SECRET"} {
		t.Setenv(key, "")
	}
	base := &http.Client{}
	got, err := AccessHTTPClient(base)
	if err != nil || got != base {
		t.Fatal("unconfigured client changed")
	}
	t.Setenv("HELMR_ACCESS_ORIGIN", "https://staging.example.com")
	if _, err := AccessHTTPClient(base); err == nil {
		t.Fatal("partial configuration accepted")
	}
	t.Setenv("HELMR_ACCESS_CLIENT_ID", "id")
	t.Setenv("HELMR_ACCESS_CLIENT_SECRET", "secret")
	if got, err := AccessHTTPClient(base); err != nil || got == base {
		t.Fatal("configured transport missing")
	}
}
