package slack

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"
)

func controlFixture() controlEnvelope {
	return controlEnvelope{Nonce: uuid.NewV7(), Action: "stop", ExpiresAt: 1780000300, Target: controlTarget{Installation: uuid.NewV7(), Thread: uuid.NewV7(), Participant: uuid.NewV7(), Environment: uuid.NewV7(), Session: uuid.NewV7()}}
}

func TestSlackControlBindsExactTargetAndExpiry(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)
	claims := controlFixture()
	now := time.Unix(1780000000, 0)
	token, err := encodeControl(key, claims)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := decodeControl(key, token, now)
	if err != nil || actual != claims {
		t.Fatalf("control changed: %+v %v", actual, err)
	}
	if _, err = decodeControl(bytes.Repeat([]byte{4}, 32), token, now); !errors.Is(err, errControlInvalid) {
		t.Fatal("wrong signer accepted")
	}
	parts := strings.Split(token, ".")
	parts[0] = "e30"
	if _, err = decodeControl(key, strings.Join(parts, "."), now); !errors.Is(err, errControlInvalid) {
		t.Fatal("target tampering accepted")
	}
	expired, err := decodeControl(key, token, time.Unix(claims.ExpiresAt, 0))
	if !errors.Is(err, errControlExpired) || expired != claims {
		t.Fatal("expiry did not distinguish authenticated historical target")
	}
	claims.Actor = "human"
	if _, err = encodeControl(key, claims); err == nil {
		t.Fatal("public Stop bound to unknown future actor")
	}
}

func TestSlackControlGestureIdentityDistinguishesTransportRetryFromNewClick(t *testing.T) {
	stop := controlFixture()
	a, err := controlRequestKey(stop, "U1", "123.001")
	if err != nil {
		t.Fatal(err)
	}
	retry, _ := controlRequestKey(stop, "U1", "123.001")
	otherClick, _ := controlRequestKey(stop, "U1", "123.002")
	otherHuman, _ := controlRequestKey(stop, "U2", "123.001")
	if a != retry || a == otherClick || a == otherHuman {
		t.Fatal("Stop gesture identity conflated retries/intent")
	}
	answer := controlFixture()
	answer.Action = "answer"
	answer.Actor = "U1"
	answer.SourceTimestamp = "1780000000.000001"
	answer.Target.Turn = uuid.NewV7()
	answer.Target.Ask = uuid.NewV7()
	first, err := controlRequestKey(answer, "U1", "")
	if err != nil {
		t.Fatal(err)
	}
	same, err := controlRequestKey(answer, "U1", "")
	if err != nil || same != first {
		t.Fatal("form retry lost nonce")
	}
	if _, err = controlRequestKey(answer, "U2", ""); err == nil {
		t.Fatal("form accepted another actor")
	}
	answer.Nonce = uuid.NewV7()
	fresh, _ := controlRequestKey(answer, "U1", "")
	if fresh == first {
		t.Fatal("new form reused old gesture")
	}
}

func TestSlackAnswerControlRequiresOpeningProvenance(t *testing.T) {
	c := controlFixture()
	c.Action, c.Actor = "answer", "U1"
	c.Target.Turn, c.Target.Ask = uuid.NewV7(), uuid.NewV7()
	for _, source := range []string{"", "invalid", "1780000300.000000", "1780000301.000000"} {
		c.SourceTimestamp = source
		if validControl(c) {
			t.Fatalf("accepted invalid opening provenance %q", source)
		}
	}
	c.SourceTimestamp = "1780000000.000001"
	token, err := encodeControl(bytes.Repeat([]byte{3}, 32), c)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeControl(bytes.Repeat([]byte{3}, 32), token, time.Unix(1780000000, 0))
	if err != nil || decoded.SourceTimestamp != c.SourceTimestamp {
		t.Fatalf("lost opening source: %+v %v", decoded, err)
	}
	c.Action, c.Actor = "open_answer", ""
	if validControl(c) {
		t.Fatal("public open control carried a fabricated opener timestamp")
	}
}
