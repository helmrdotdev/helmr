package bootgate

import (
	"bytes"
	"testing"
	"time"
)

func TestBootstrapRequiresCompleteFreshInput(t *testing.T) {
	request := Request{Time: time.Unix(100, 42), Entropy: [EntropyBytes]byte{7}, Challenge: [ChallengeBytes]byte{9}}
	raw := request.Encode()
	got, err := ReadRequest(bytes.NewReader(raw))
	if err != nil || !got.Time.Equal(request.Time) || got.Entropy != request.Entropy || got.Challenge != request.Challenge {
		t.Fatalf("bootstrap input differs: %v", err)
	}
	for length := range len(raw) {
		if _, err := ReadRequest(bytes.NewReader(raw[:length])); err == nil {
			t.Fatalf("accepted truncated input at %d", length)
		}
	}
	for _, invalid := range []Request{
		{Time: time.Unix(0, 0), Entropy: request.Entropy, Challenge: request.Challenge},
		{Time: request.Time, Challenge: request.Challenge},
		{Time: request.Time, Entropy: request.Entropy},
	} {
		if _, err := ReadRequest(bytes.NewReader(invalid.Encode())); err == nil {
			t.Fatal("accepted missing freshness input")
		}
	}
}
