package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func signedHeaders(secret, body []byte, at time.Time) http.Header {
	stamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v0:" + stamp + ":"))
	mac.Write(body)
	return http.Header{"X-Slack-Request-Timestamp": []string{stamp}, "X-Slack-Signature": []string{"v0=" + hex.EncodeToString(mac.Sum(nil))}}
}

func TestSlackSignatureRequiresRecentExactRawBody(t *testing.T) {
	secret := []byte("fixture-signing-secret")
	body := []byte(`payload=%7B%22text%22%3A%22hello%2Bworld%22%7D`)
	now := time.Unix(1780000000, 0)
	headers := signedHeaders(secret, body, now)
	if !VerifyRequest(secret, headers, body, now) {
		t.Fatal("valid signature rejected")
	}
	if VerifyRequest(secret, headers, []byte(`payload={"text":"hello+world"}`), now) {
		t.Fatal("parsed/re-encoded body substituted")
	}
	if VerifyRequest([]byte("other"), headers, body, now) || VerifyRequest(nil, headers, body, now) {
		t.Fatal("wrong/missing key accepted")
	}
	for _, offset := range []time.Duration{-301 * time.Second, 301 * time.Second} {
		if VerifyRequest(secret, signedHeaders(secret, body, now.Add(offset)), body, now) {
			t.Fatal("stale/future replay accepted")
		}
	}
	duplicate := headers.Clone()
	duplicate.Add("X-Slack-Signature", headers.Get("X-Slack-Signature"))
	if VerifyRequest(secret, duplicate, body, now) {
		t.Fatal("ambiguous signature headers accepted")
	}
	for _, stamp := range []string{"9223372036854775807", "-9223372036854775808", "+1780000000", "not-a-timestamp"} {
		malformed := headers.Clone()
		malformed.Set("X-Slack-Request-Timestamp", stamp)
		if VerifyRequest(secret, malformed, body, now) {
			t.Fatal("invalid timestamp accepted")
		}
	}
}
