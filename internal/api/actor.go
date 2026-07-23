package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/publicid"
)

var actorDeclaredIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type SendActorInputRequest struct {
	ActorID        string          `json:"actor_id,omitempty"`
	ActorKey       string          `json:"actor_key,omitempty"`
	Input          json.RawMessage `json:"input"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type SendActorInputResponse struct {
	Sequence int64 `json:"sequence"`
}

func ValidateActorDeclaredID(id string) error {
	if !actorDeclaredIDPattern.MatchString(id) {
		return fmt.Errorf("actor declared ID %q must match %s", id, actorDeclaredIDPattern.String())
	}
	return nil
}

func ValidateActorPublicID(id string) error {
	if err := publicid.ValidateFor(publicid.Actor, id); err != nil {
		return fmt.Errorf("invalid actor ID: %w", err)
	}
	return nil
}

func ValidateActorKey(key string) error {
	if !utf8.ValidString(key) {
		return errors.New("actor_key must be valid UTF-8")
	}
	if len(key) == 0 || len(key) > 512 {
		return errors.New("actor_key must be between 1 and 512 bytes")
	}
	if strings.IndexByte(key, 0) >= 0 {
		return errors.New("actor_key must not contain NUL")
	}
	first, _ := utf8.DecodeRuneInString(key)
	last, _ := utf8.DecodeLastRuneInString(key)
	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return errors.New("actor_key must not start or end with whitespace")
	}
	return nil
}

func ValidateSendActorInputRequest(request SendActorInputRequest) error {
	hasID := request.ActorID != ""
	hasKey := request.ActorKey != ""
	if hasID == hasKey {
		return errors.New("exactly one of actor_id or actor_key is required")
	}
	if hasID {
		if err := ValidateActorPublicID(request.ActorID); err != nil {
			return err
		}
	} else if err := ValidateActorKey(request.ActorKey); err != nil {
		return err
	}
	if len(request.Input) == 0 {
		return errors.New("input is required")
	}
	if _, err := jsoncanon.Transform(request.Input); err != nil {
		return fmt.Errorf("input must be unambiguous I-JSON: %w", err)
	}
	return nil
}
