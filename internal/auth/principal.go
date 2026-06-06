package auth

import (
	"errors"

	"github.com/google/uuid"
)

const (
	principalPrefixAPIKey = "api_key:"
	principalPrefixSystem = "system"
	principalPrefixUser   = "user:"
)

func ActorPrincipal(actor Actor) (string, error) {
	switch actor.Kind {
	case ActorKindSession:
		if actor.UserID == uuid.Nil {
			return "", errors.New("user identity is required")
		}
		return principalPrefixUser + actor.UserID.String(), nil
	case ActorKindAPIKey:
		if actor.APIKeyID == uuid.Nil {
			return "", errors.New("api key identity is required")
		}
		return principalPrefixAPIKey + actor.APIKeyID.String(), nil
	default:
		return "", errors.New("supported actor identity is required")
	}
}

func ActorPrincipalAllowSystem(actor Actor) (string, error) {
	if actor.Kind == ActorKindSystem {
		return principalPrefixSystem, nil
	}
	return ActorPrincipal(actor)
}
