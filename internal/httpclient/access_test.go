package httpclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAccessServiceTokenOriginBoundary(t *testing.T) {
	var targetHeaders http.Header
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHeaders = r.Header.Clone(); w.WriteHeader(200) }))
	defer target.Close()
	var originHeaders http.Header
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHeaders = r.Header.Clone()
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()
	client, err := WithAccessServiceToken(origin.Client(), origin.URL, "client-id", "client-secret")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, origin.URL, nil)
	req.Header.Set("Authorization", "Bearer application-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if originHeaders.Get("CF-Access-Client-Id") != "client-id" || originHeaders.Get("CF-Access-Client-Secret") != "client-secret" || originHeaders.Get("Authorization") != "Bearer application-token" {
		t.Fatal("origin authentication missing")
	}
	if targetHeaders.Get("CF-Access-Client-Id") != "" || targetHeaders.Get("CF-Access-Client-Secret") != "" {
		t.Fatal("Access credential leaked on redirect")
	}
	if req.Header.Get("CF-Access-Client-Secret") != "" {
		t.Fatal("caller request mutated")
	}
	// A direct signed upload using this client must also omit the outer credentials.
	resp, err = client.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if targetHeaders.Get("CF-Access-Client-Secret") != "" {
		t.Fatal("Access credential leaked on direct request")
	}
}

func TestAccessServiceTokenRejectsInvalidConfiguration(t *testing.T) {
	for _, origin := range []string{"http://example.com", "https://user@example.com", "https://example.com/path", "https://example.com?secret=value", ""} {
		if _, err := WithAccessServiceToken(nil, origin, "id", "secret"); err == nil {
			t.Fatalf("accepted invalid origin %q", origin)
		}
	}
	if _, err := WithAccessServiceToken(nil, "https://example.com", "", "secret"); err == nil {
		t.Fatal("accepted missing ID")
	}
}

func TestAccessServiceTokenRejectsHostOverride(t *testing.T) {
	var got http.Header
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer server.Close()
	client, err := WithAccessServiceToken(server.Client(), server.URL, "id", "secret")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	req.Host = "other.example"
	req.Header.Set("CF-Access-Client-Secret", "stale")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if got.Get("CF-Access-Client-Secret") != "" || got.Get("CF-Access-Client-Id") != "" {
		t.Fatal("credential sent for a different virtual host")
	}
}
