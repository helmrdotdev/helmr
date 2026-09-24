package workerclient

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestInitialComputerKeyRejectsMalformedAndSensitiveErrors(t *testing.T) {
	valid := workerapi.ComputerKeyMaterial{Scope: "computer-scope", ID: uuid.NewV7().String(), Key: bytes.Repeat([]byte{0x61}, 32)}
	encoded, _ := json.Marshal(valid)
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"error-body", 503, `{"error":{"message":"SECRET-MARKER"}}`},
		{"partial", 200, string(encoded[:len(encoded)-1]) + `,"other":"SECRET-MARKER"`},
		{"trailing", 200, string(encoded) + "SECRET-MARKER"},
		{"oversized", 200, string(encoded) + strings.Repeat(" ", 1100)},
		{"wrong-size", 200, `{"scope":"scope","id":"` + valid.ID + `","key":"YQ=="}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/worker/v1/instance/token" {
					_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{Token: "token", ExpiresInSeconds: 3600})
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			c, err := New(server.URL, WithAuth(uuid.NewV7().String(), "fixture-secret"), WithService(uuid.NewV7().String()))
			if err != nil {
				t.Fatal(err)
			}
			material, err := c.InitialComputerKey(t.Context(), workerapi.InitialComputerKeyRequest{})
			if err == nil || len(material.Key) != 0 || strings.Contains(err.Error(), "SECRET-MARKER") {
				t.Fatal("sensitive malformed response not suppressed")
			}
		})
	}
}

func TestInitialComputerKeyRefreshesAuthenticationOnce(t *testing.T) {
	tokenCalls, keyCalls := 0, 0
	valid := workerapi.ComputerKeyMaterial{Scope: "scope", ID: uuid.NewV7().String(), Key: make([]byte, 32)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/instance/token" {
			tokenCalls++
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{Token: "token", ExpiresInSeconds: 3600})
			return
		}
		keyCalls++
		if keyCalls == 1 {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(valid)
	}))
	defer server.Close()
	c, err := New(server.URL, WithAuth(uuid.NewV7().String(), "fixture-secret"), WithService(uuid.NewV7().String()))
	if err != nil {
		t.Fatal(err)
	}
	material, err := c.InitialComputerKey(t.Context(), workerapi.InitialComputerKeyRequest{})
	defer clear(material.Key)
	if err != nil || tokenCalls != 2 || keyCalls != 2 || len(material.Key) != 32 {
		t.Fatalf("refresh failed: token=%d key=%d err=%v", tokenCalls, keyCalls, err)
	}
}
