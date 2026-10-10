package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// VerifyRequest authenticates the exact raw request before JSON/form decoding or
// durable receipt. It grants no Helmr identity, membership or operation authority.
func VerifyRequest(secret []byte, headers http.Header, body []byte, now time.Time) bool {
	timestamps, signatures := headers.Values("X-Slack-Request-Timestamp"), headers.Values("X-Slack-Signature")
	if len(secret) == 0 || len(timestamps) != 1 || len(signatures) != 1 {
		return false
	}
	stamp, signature := timestamps[0], signatures[0]
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || strconv.FormatInt(seconds, 10) != stamp || seconds < now.Unix()-300 || seconds > now.Unix()+300 || !strings.HasPrefix(signature, "v0=") {
		return false
	}
	expected, err := hex.DecodeString(strings.TrimPrefix(signature, "v0="))
	if err != nil || len(expected) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v0:" + stamp + ":"))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), expected)
}
