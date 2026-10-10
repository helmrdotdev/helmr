package slack

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

var errControlInvalid = errors.New("invalid Slack control")
var errControlExpired = errors.New("expired Slack control")

// Public controls identify exact adapter/core owners, never a capability secret.
// The receiving transaction rechecks all containment, identity and authority.
type controlTarget struct {
	Installation uuid.UUID `json:"installation"`
	Thread       uuid.UUID `json:"thread"`
	Participant  uuid.UUID `json:"participant"`
	Environment  uuid.UUID `json:"environment"`
	Session      uuid.UUID `json:"session"`
	Turn         uuid.UUID `json:"turn"`
	Ask          uuid.UUID `json:"ask"`
}

type controlEnvelope struct {
	Nonce           uuid.UUID     `json:"nonce"`
	Action          string        `json:"action"`
	Target          controlTarget `json:"target"`
	Actor           string        `json:"actor,omitempty"`
	ExpiresAt       int64         `json:"expires_at"`
	SourceTimestamp string        `json:"source_ts,omitempty"`
}

func validControl(c controlEnvelope) bool {
	t := c.Target
	if c.Nonce == uuid.Nil() || t.Installation == uuid.Nil() || t.Thread == uuid.Nil() || t.Participant == uuid.Nil() || t.Environment == uuid.Nil() || t.Session == uuid.Nil() || c.ExpiresAt <= 0 {
		return false
	}
	switch c.Action {
	case "stop":
		return t.Turn == uuid.Nil() && t.Ask == uuid.Nil() && c.Actor == "" && c.SourceTimestamp == ""
	case "open_answer":
		return t.Turn != uuid.Nil() && t.Ask != uuid.Nil() && c.Actor == "" && c.SourceTimestamp == ""
	case "answer":
		source, err := slackMessageTime(c.SourceTimestamp)
		return err == nil && source.Before(time.Unix(c.ExpiresAt, 0)) && t.Turn != uuid.Nil() && t.Ask != uuid.Nil() && c.Actor != "" && len(c.Actor) <= 128
	default:
		return false
	}
}

func controlMAC(key []byte, payload string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("helmr:slack-control:"))
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

func encodeControl(key []byte, claims controlEnvelope) (string, error) {
	if len(key) < 32 || !validControl(claims) {
		return "", errControlInvalid
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(body)
	value := encoded + "." + base64.RawURLEncoding.EncodeToString(controlMAC(key, encoded))
	if len(value) > 2000 {
		return "", errControlInvalid
	}
	return value, nil
}

// An expired but authentic control returns claims with errControlExpired only so
// a caller can reject the expired gesture or look up an already-accepted receipt
// after current viewer checks. It never authorizes fresh core admission. Invalid
// signatures return no claims.
func decodeControl(key []byte, value string, now time.Time) (controlEnvelope, error) {
	var claims controlEnvelope
	if len(key) < 32 || len(value) > 2000 {
		return claims, errControlInvalid
	}
	payload, signature, found := strings.Cut(value, ".")
	if !found {
		return claims, errControlInvalid
	}
	mac, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || !hmac.Equal(controlMAC(key, payload), mac) {
		return claims, errControlInvalid
	}
	body, err := base64.RawURLEncoding.Strict().DecodeString(payload)
	if err != nil {
		return claims, errControlInvalid
	}
	body, err = jsoncanon.Transform(body)
	if err != nil {
		return claims, errControlInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&claims) != nil || !validControl(claims) {
		return controlEnvelope{}, errControlInvalid
	}
	if claims.ExpiresAt <= now.Unix() {
		return claims, errControlExpired
	}
	return claims, nil
}

// Direct-click identity includes actor and action time. Another deliberate click
// is a new gesture; duplicate transport retries retain the same receipt key.
// The form nonce was issued for its authenticated actor when the form was opened.
func controlRequestKey(claims controlEnvelope, actor, actionTimestamp string) (string, error) {
	if !validControl(claims) || actor == "" || len(actor) > 128 {
		return "", errControlInvalid
	}
	if claims.Action == "answer" {
		if actor != claims.Actor {
			return "", errControlInvalid
		}
		return "form:" + claims.Nonce.String(), nil
	}
	if (claims.Action != "stop" && claims.Action != "open_answer") || !timestampPattern.MatchString(actionTimestamp) {
		return "", errControlInvalid
	}
	data, _ := json.Marshal([]string{claims.Nonce.String(), actor, actionTimestamp})
	digest := sha256.Sum256(data)
	return "control:" + hex.EncodeToString(digest[:]), nil
}
